#!/usr/bin/env node

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';

const [baseURL, credentialsPath] = process.argv.slice(2);
if (!baseURL || !credentialsPath) {
  console.error('usage: node scripts/replication-drill-acceptance.mjs BASE_URL CREDENTIALS_PATH');
  process.exit(2);
}
const credentials = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const base = baseURL.replace(/\/$/, '');
const primary = 'dbsphere-replication-drill-primary-1';
const standby = 'dbsphere-replication-drill-standby-1';
let cookie = '';
let csrf = '';
let paused = false;
let completed = false;

function psql(container, sql) {
  return execFileSync('docker', ['exec', container, 'psql', '-U', 'postgres', '-Atc', sql],
    { encoding: 'utf8', timeout: 120000 }).trim();
}
async function api(path, method = 'GET', payload) {
  const response = await fetch(base + path, {
    method, headers: {
      ...(payload ? { 'Content-Type': 'application/json' } : {}),
      ...(cookie ? { Cookie: cookie } : {}),
      ...(method !== 'GET' && csrf ? { 'X-CSRF-Token': csrf } : {})
    }, body: payload ? JSON.stringify(payload) : undefined,
    signal: AbortSignal.timeout(45000)
  });
  const body = await response.json();
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${JSON.stringify(body)}`);
  return { body, response };
}
async function check(id) {
  const result = (await api(`/api/assets/${id}/check`, 'POST')).body.result;
  assert.equal(result.status, 'ok', `PostgreSQL collection failed: ${result.error || 'unknown'}`);
  const sample = (await api(`/api/assets/${id}/samples/${result.sampleId}`)).body.sample;
  assert.equal(sample.status, 'ok');
  return { result, sample };
}
function lagBytes() {
  const raw = psql(primary, `SELECT state,
    CASE WHEN replay_lsn IS NOT NULL THEN
      greatest(0,pg_wal_lsn_diff(pg_current_wal_lsn(),replay_lsn))::bigint END
    FROM pg_stat_replication`);
  const [state, bytes] = raw.split('|');
  return state === 'streaming' && /^\d+$/.test(bytes) ? Number(bytes) : NaN;
}
async function until(description, condition, timeoutMs = 90000) {
  const started = Date.now();
  while (Date.now() - started < timeoutMs) {
    if (await condition()) return;
    await delay(3000);
  }
  throw new Error(`Timed out waiting for ${description}`);
}

const ready = (await api('/readyz')).body;
assert.equal(ready.storage, 'postgresql18');
assert.equal(psql(primary, 'SELECT count(*) FROM pg_stat_replication'), '1');
assert.equal(psql(standby, 'SELECT pg_is_in_recovery()'), 't');
assert.equal(psql(standby, 'SELECT status FROM pg_stat_wal_receiver'), 'streaming');
const login = await api('/api/login', 'POST', { password: credentials.adminPassword });
cookie = login.response.headers.get('set-cookie')?.split(';')[0] || '';
csrf = login.body.csrf;
assert.ok(cookie && csrf);

const runName = `replication-drill-${Date.now()}`;
const createAsset = async (name, host) => {
  const created = (await api('/api/assets', 'POST', {
    name: `${runName}-${name}`, host, port: 5432, database: 'postgres',
    username: 'dbsphere_monitor', password: credentials.monitorPassword,
    sslMode: 'disable', runtime: 'linux'
  })).body.asset;
  assert.ok(created.id);
  return created.id;
};
const primaryId = await createAsset('primary', 'primary');
const standbyId = await createAsset('standby', 'standby');
const baselinePrimary = (await check(primaryId)).sample;
const baselineStandby = (await check(standbyId)).sample;
assert.equal(baselinePrimary.metrics.role, 'primary');
assert.equal(baselinePrimary.metrics.replicas, 1);
assert.equal(baselinePrimary.metrics.replication_observed, true);
assert.equal(baselinePrimary.metrics.monitor_role, true);
assert.equal(baselinePrimary.metrics.privileged_role, false);
assert.equal(baselineStandby.metrics.role, 'standby');
assert.equal(baselineStandby.metrics.wal_receiver, 'streaming');
assert.equal(baselinePrimary.metrics.diagnostics.replicationLinks.data.links.length, 1);
assert.match(baselinePrimary.metrics.diagnostics.walPosition.data.timeline, /^[0-9A-F]{8}$/);
assert.equal(baselineStandby.metrics.diagnostics.walReceiver.data.status, 'streaming');

let eventId = '';
let faultSampleId = '';
try {
  psql(standby, 'SELECT pg_wal_replay_pause()');
  paused = true;
  await until('standby replay pause', () => psql(standby, 'SELECT pg_is_wal_replay_paused()') === 't');
  // A switch without a new WAL record can be a no-op after the first segment.
  psql(primary, `DO $$ DECLARE i integer; BEGIN FOR i IN 1..8 LOOP
    PERFORM pg_create_restore_point('dbsphere_drill');
    PERFORM pg_switch_wal();
  END LOOP; END $$;`);
  await until('64 MiB replication lag', () => lagBytes() >= 64 * 1024 * 1024, 180000);
  const first = await check(primaryId);
  assert.ok(Number.isFinite(first.sample.metrics.replication_lag_bytes) &&
    first.sample.metrics.replication_lag_bytes >= 64 * 1024 * 1024);
  const link = first.sample.metrics.diagnostics.replicationLinks.data.links[0];
  assert.ok(link.replayGapBytes >= 64 * 1024 * 1024);
  for (const point of ['sentLsn', 'writeLsn', 'flushLsn', 'replayLsn']) assert.match(link[point], /^[0-9A-F]+\/[0-9A-F]+$/);
  const range = new URLSearchParams({ from: baselinePrimary.collectedAt, to: new Date().toISOString() });
  const history = (await api(`/api/assets/${primaryId}/replication?${range}`)).body;
  assert.equal(history.observations[0].id, first.sample.id);
  assert.equal(history.observations[0].links.data.links[0].replayLsn, link.replayLsn);
  await delay(125000); // Default pg_replication_lag rule requires 120 s.
  const fault = await check(primaryId);
  faultSampleId = fault.result.sampleId;
  assert.ok(Number.isFinite(fault.sample.metrics.replication_lag_bytes) &&
    fault.sample.metrics.replication_lag_bytes >= 64 * 1024 * 1024);

  const events = (await api('/api/events')).body.events.filter(event => event.assetId === primaryId);
  assert.equal(events.length, 1, 'expected one replication incident');
  eventId = events[0].id;
  assert.equal(events[0].status, 'open');
  const detail = (await api(`/api/events/${eventId}`)).body;
  assert.equal(detail.event.id, eventId);
  const candidate = detail.analysis.candidates.find(item => item.code === 'replication_lag');
  assert.ok(candidate, 'replication lag root-cause candidate missing');
  assert.ok(candidate.supports.some(id => id === `sample:${faultSampleId}`));
  assert.ok(detail.event.evidence.some(item => item.kind === 'alert_firing'));
  assert.ok(detail.analysis.graph.nodes.some(item => item.id === `sample:${faultSampleId}`));
  const reportResponse = await fetch(`${base}/api/events/${eventId}/report?format=md`,
    { headers: { Cookie: cookie }, signal: AbortSignal.timeout(45000) });
  assert.equal(reportResponse.status, 200);
  const report = await reportResponse.text();
  assert.ok(report.includes(eventId) && report.includes('replication_lag') && report.includes(faultSampleId),
    'same-event report lacks the replication evidence');

  psql(standby, 'SELECT pg_wal_replay_resume()');
  paused = false;
  await until('standby replay catch-up', () => lagBytes() < 64 * 1024 * 1024, 180000);
  const recovered = await check(primaryId);
  assert.equal(recovered.sample.metrics.replication_observed, true);
  assert.equal(recovered.sample.metrics.replicas, 1);
  assert.ok(Number.isFinite(recovered.sample.metrics.replication_lag_bytes) &&
    recovered.sample.metrics.replication_lag_bytes < 64 * 1024 * 1024);
  const final = (await api(`/api/events/${eventId}`)).body;
  assert.equal(final.event.status, 'resolved');
  assert.ok(final.event.evidence.some(item => item.kind === 'alert_recovered' &&
    item.payload?.sampleId === recovered.result.sampleId));
  assert.ok(final.event.evidence.some(item => item.kind === 'alert_firing'));
  assert.ok(final.analysis.candidates.some(item => item.code === 'replication_lag'));
  completed = true;
  console.log(JSON.stringify({ eventId, primaryId, standbyId, faultSampleId,
    recoverySampleId: recovered.result.sampleId, status: final.event.status,
    faultLagBytes: fault.sample.metrics.replication_lag_bytes,
    recoveryLagBytes: recovered.sample.metrics.replication_lag_bytes,
    replicasAtRecovery: recovered.sample.metrics.replicas,
    candidate: candidate.code, storage: ready.storage }, null, 2));
} finally {
  if (paused) {
    try { psql(standby, 'SELECT pg_wal_replay_resume()'); }
    catch (error) { console.error(`Could not resume test standby automatically: ${error.message}`); }
  }
  if (!completed && eventId) {
    try {
      await until('test standby recovery after failure', () => lagBytes() < 64 * 1024 * 1024, 180000);
      await check(primaryId);
    } catch (error) {
      console.error(`Could not close the test incident automatically: ${error.message}`);
    }
  }
}
