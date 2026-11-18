#!/usr/bin/env node

// Real PG18 WAL archiver failure against the isolated replication drill store.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';

const [baseURL, credentialsPath] = process.argv.slice(2);
if (!baseURL || !credentialsPath) {
  console.error('usage: node scripts/wal-archive-drill-acceptance.mjs BASE_URL CREDENTIALS_PATH');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const credentials = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const primary = 'dbsphere-replication-drill-primary-1';
let cookie = '';
let csrf = '';
let archiveCommandChanged = false;

function psql(sql) {
  return execFileSync('docker', ['exec', primary, 'psql', '-U', 'postgres', '-Atc', sql],
    { encoding: 'utf8', timeout: 120000 }).trim();
}
async function api(path, method = 'GET', payload) {
  const response = await fetch(base + path, {
    method,
    headers: {
      ...(payload ? { 'Content-Type': 'application/json' } : {}),
      ...(cookie ? { Cookie: cookie } : {}),
      ...(method !== 'GET' && csrf ? { 'X-CSRF-Token': csrf } : {})
    },
    body: payload ? JSON.stringify(payload) : undefined,
    signal: AbortSignal.timeout(45000)
  });
  const body = await response.json();
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${JSON.stringify(body)}`);
  return { body, response };
}
async function sample(assetId) {
  const { result } = (await api(`/api/assets/${assetId}/check`, 'POST')).body;
  assert.equal(result.status, 'ok');
  const { sample: row } = (await api(`/api/assets/${assetId}/samples/${result.sampleId}`)).body;
  assert.equal(row.status, 'ok');
  assert.equal(row.metrics.diagnostics?.archiver?.status, 'ok');
  return { id: result.sampleId, metrics: row.metrics };
}
async function until(label, check, timeoutMs = 120000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const value = await check();
    if (value) return value;
    await delay(3000);
  }
  throw new Error(`timed out waiting for ${label}`);
}
function failedCount() { return Number(psql('SELECT failed_count FROM pg_stat_archiver')); }
function archivedCount() { return Number(psql('SELECT archived_count FROM pg_stat_archiver')); }
function switchWAL() {
  psql("SELECT pg_create_restore_point('dbsphere_archive_drill'); SELECT pg_switch_wal()");
}

assert.equal((await api('/readyz')).body.storage, 'postgresql18');
assert.equal(psql('SHOW archive_mode'), 'on');
if (psql('SHOW archive_command') !== 'true') {
  psql("ALTER SYSTEM SET archive_command = 'true'");
  assert.equal(psql('SELECT pg_reload_conf()'), 't');
  await until('healthy test archive command', () => psql('SHOW archive_command') === 'true');
}
const login = await api('/api/login', 'POST', { password: credentials.adminPassword });
cookie = login.response.headers.get('set-cookie')?.split(';')[0] || '';
csrf = login.body.csrf;
assert.ok(cookie && csrf);
const asset = (await api('/api/assets', 'POST', {
  name: `PG18 WAL archive drill ${Date.now()}`, host: 'primary', port: 5432,
  database: 'postgres', username: 'dbsphere_monitor', password: credentials.monitorPassword,
  sslMode: 'disable', runtime: 'linux'
})).body.asset;
assert.ok(asset.id);
const baseline = await sample(asset.id);
assert.equal(baseline.metrics.monitor_role, true);
assert.equal(baseline.metrics.privileged_role, false);
const baselineFailed = failedCount();
assert.equal(Number(baseline.metrics.diagnostics.archiver.data.failedCount), baselineFailed);

let eventId = '';
let failure;
let recovery;
try {
  psql("ALTER SYSTEM SET archive_command = 'false'");
  archiveCommandChanged = true;
  assert.equal(psql('SELECT pg_reload_conf()'), 't');
  await until('test archiver command reload', () => psql('SHOW archive_command') === 'false');
  switchWAL();
  await until('real pg_stat_archiver failure', () => failedCount() > baselineFailed);
  failure = await sample(asset.id);
  const count = Number(failure.metrics.diagnostics.archiver.data.failedCount);
  assert.ok(count > baselineFailed);
  const events = (await api('/api/events')).body.events.filter(row => row.assetId === asset.id);
  assert.equal(events.length, 1, 'expected one WAL storage incident');
  eventId = events[0].id;
  assert.equal(events[0].status, 'open');
  const detail = (await api(`/api/events/${eventId}`)).body;
  assert.equal(detail.event.eventGroup, 'storage');
  assert.ok(detail.event.evidence.some(row => row.kind === 'alert_firing' &&
    row.payload?.ruleId === 'pg_archive_failures' && row.payload?.sampleId === failure.id));
  const candidate = detail.analysis.candidates.find(row => row.code === 'wal_archive_failures');
  assert.ok(candidate?.supports.includes(`sample:${failure.id}`));
  assert.ok(detail.analysis.graph.nodes.some(row => row.id === `sample:${failure.id}`));
  const reportResponse = await fetch(`${base}/api/events/${eventId}/report?format=md`,
    { headers: { Cookie: cookie }, signal: AbortSignal.timeout(20000) });
  assert.equal(reportResponse.status, 200);
  const report = await reportResponse.text();
  assert.ok(report.includes(eventId) && report.includes('wal_archive_failures') && report.includes(failure.id));

  const archivedBefore = archivedCount();
  psql("ALTER SYSTEM SET archive_command = 'true'");
  assert.equal(psql('SELECT pg_reload_conf()'), 't');
  await until('test archiver command restoration', () => psql('SHOW archive_command') === 'true');
  archiveCommandChanged = false;
  await until('WAL archive resumed', () => archivedCount() > archivedBefore);
  recovery = await until('same event recovery after zero new archive failures', async () => {
    const observed = await sample(asset.id);
    const current = (await api(`/api/events/${eventId}`)).body;
    return current.event.status === 'resolved' ? observed : null;
  });
  const final = (await api(`/api/events/${eventId}`)).body;
  assert.ok(final.event.evidence.some(row => row.kind === 'alert_recovered' &&
    row.payload?.ruleId === 'pg_archive_failures' && row.payload?.sampleId === recovery.id));
  console.log(JSON.stringify({ eventId, assetId: asset.id, failureSampleId: failure.id,
    recoverySampleId: recovery.id, baselineFailed, failedAtIncident: count,
    failedAtRecovery: failedCount(), archivedAtRecovery: archivedCount(),
    status: final.event.status, candidate: candidate.code, storage: 'postgresql18' }, null, 2));
} finally {
  if (archiveCommandChanged) {
    try {
      psql("ALTER SYSTEM SET archive_command = 'true'");
      psql('SELECT pg_reload_conf()');
      if (eventId) await sample(asset.id);
    } catch (error) {
      console.error(`Could not restore the isolated test archiver automatically: ${error.message}`);
    }
  }
}
