#!/usr/bin/env node

// Real three-node Patroni election with live read/write and standby entrances.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';

const [baseURL, credentialsPath, envPath, resultPath, major = '18'] = process.argv.slice(2);
if (!baseURL || !credentialsPath || !envPath || !resultPath) {
  console.error('usage: node scripts/patroni-drill-acceptance.mjs BASE_URL CREDENTIALS_PATH ENV_PATH RESULT_PATH [PG_MAJOR]');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const credentials = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const compose = ['compose', '--env-file', envPath, '-f', 'test/patroni-drill/compose.yaml'];
let cookie = '';
let csrf = '';

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
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${JSON.stringify(body)}`);
  return { body, response };
}
function container(service) {
  const id = execFileSync('docker', [...compose, 'ps', '-q', service],
    { encoding: 'utf8', timeout: 20000 }).trim();
  assert.ok(id, `missing ${service} container`);
  return id;
}
function docker(...args) {
  return execFileSync('docker', args, { encoding: 'utf8', timeout: 60000 }).trim();
}
function maintenance(service, action) {
  return docker('exec', container(service), 'patronictl', '-c', '/tmp/dbsphere-patroni.yml',
    action, 'dbsphere-ci-patroni', ...(action === 'pause' ? ['--wait'] : []));
}
async function cluster() {
  const response = await fetch(`http://127.0.0.1:${process.env.DBSPHERE_DRILL_REST_PORT || 18018}/cluster`,
    { signal: AbortSignal.timeout(10000) });
  assert.equal(response.status, 200);
  return response.json();
}
async function sample(assetId) {
  const result = (await api(`/api/assets/${assetId}/check`, 'POST')).body.result;
  const row = (await api(`/api/assets/${assetId}/samples/${result.sampleId}`)).body.sample;
  return { id: result.sampleId, collectedAt: row.collectedAt, status: result.status, metrics: row.metrics };
}
async function waitUntil(description, predicate, timeoutMs = 150000) {
  const until = Date.now() + timeoutMs;
  while (Date.now() < until) {
    const value = await predicate();
    if (value) return value;
    await delay(3000);
  }
  throw new Error(`timed out waiting for ${description}`);
}

assert.equal((await api('/readyz')).body.storage, 'postgresql18');
await waitUntil('one leader and two streaming replicas', async () => {
  const members = (await cluster()).members;
  return members.length === 3 && members.some(item => item.role === 'leader' && item.state === 'running') &&
    members.some(item => item.role === 'sync_standby' && item.state === 'streaming') &&
    members.some(item => item.name === 'patroni3' && item.role === 'replica' && item.state === 'streaming' && item.tags?.nosync);
});
const members = (await cluster()).members;
const leader = members.find(item => item.role === 'leader').name;
const survivor = members.find(item => item.role === 'sync_standby').name;
assert.ok(['patroni1', 'patroni2'].includes(leader));
assert.ok(['patroni1', 'patroni2'].includes(survivor));
execFileSync('docker', ['exec', '-i', container(leader), 'psql', '-U', 'postgres',
  '-v', 'ON_ERROR_STOP=1'], {
  input: `DO $test$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dbsphere_monitor') THEN
      CREATE ROLE dbsphere_monitor LOGIN PASSWORD '${credentials.monitorPassword}';
    ELSE
      ALTER ROLE dbsphere_monitor PASSWORD '${credentials.monitorPassword}';
    END IF;
  END $test$;
  GRANT pg_monitor TO dbsphere_monitor;\n`,
  encoding: 'utf8', timeout: 30000
});

const login = await api('/api/login', 'POST', { password: credentials.adminPassword });
cookie = login.response.headers.get('set-cookie')?.split(';')[0] || '';
csrf = login.body.csrf;
assert.ok(cookie && csrf);
const asset = (await api('/api/assets', 'POST', {
  name: `Patroni election ${Date.now()}`, clusterName: 'Patroni 三节点验收', host: leader, port: 5432, database: 'postgres',
  username: 'dbsphere_monitor', password: credentials.monitorPassword,
  sslMode: 'disable', runtime: 'linux', patroniUrl: `http://${survivor}:8008`
})).body.asset;
assert.ok(asset.id && asset.patroniConfigured);
const { body: routes } = await api(`/api/assets/${asset.id}/routing`, 'PATCH',
  {kind: 'haproxy', entry: 'proxy:5432', expectedRole: 'primary', readEntry: 'proxy:5433'});
assert.equal(routes.readEntry, 'proxy:5433');
const baseline = await waitUntil('healthy real read/write and standby routes', async () => {
  const result = await sample(asset.id);
  return result.status === 'ok' && result.metrics.routing?.status === 'ok' && result.metrics.routing?.readStatus === 'ok'
    ? result : null;
});
assert.equal(baseline.status, 'ok');
assert.equal(Math.floor(baseline.metrics.version_num / 10000), Number(major));
assert.equal(baseline.metrics.patroni?.leaderCount, 1);
assert.equal(baseline.metrics.patroni?.replicaCount, 2);
assert.ok(baseline.metrics.patroni.nodeState?.dcsLastSeen);
assert.equal(baseline.metrics.patroni.heartbeat?.status, 'healthy');
assert.equal(baseline.metrics.routing.probe.role, 'primary');
assert.equal(baseline.metrics.routing.readProbe.role, 'standby');
assert.ok(baseline.metrics.patroni.members.some(item => item.name === 'patroni3' && item.nosync));
assert.equal(baseline.metrics.monitor_role, true);
assert.equal(baseline.metrics.privileged_role, false);
assert.ok(!JSON.stringify(baseline).includes(credentials.monitorPassword));
console.log(JSON.stringify({ phase: 'baseline', targetPGMajor: Number(major), leader, survivor,
  sampleId: baseline.id }));

const leaderContainer = container(leader);
let stopped = false;
let ruleChanged = false;
let paused = false;
let noLeaderSample;
let unavailableSample;
let promoted;
let recovery;
try {
  await api('/api/alert-rules/patroni_leader_invalid', 'PATCH', { durationSeconds: 0 });
  ruleChanged = true;
  maintenance(survivor, 'pause');
  paused = true;
  docker('kill', leaderContainer);
  stopped = true;
  const until = Date.now() + 120000;
  while (Date.now() < until) {
    let observed;
    try { observed = await sample(asset.id); }
    catch (error) {
      console.error(`Transient fault-window sample failure: ${error.message}`);
      await delay(2500);
      continue;
    }
    if (observed.metrics.patroni?.status === 'ok' && observed.metrics.patroni.leaderCount === 0) {
      if (!noLeaderSample) {
        noLeaderSample = observed;
        console.log(JSON.stringify({ phase: 'no-leader', sampleId: observed.id }));
      }
    }
    if (observed.status === 'error' && observed.metrics.collector?.pgStatus === 'availability') {
      if (!unavailableSample) {
        unavailableSample = observed;
        console.log(JSON.stringify({ phase: 'pg-unavailable', sampleId: observed.id }));
      }
    }
    if (noLeaderSample && unavailableSample) break;
    await delay(2500);
  }
  assert.ok(noLeaderSample, 'real leader election did not yield a no-leader sample');
  assert.ok(unavailableSample, 'stopped leader did not yield a PostgreSQL availability sample');
  assert.notEqual(noLeaderSample.metrics.routing.status, 'ok', 'write entry should reflect the unlocked cluster');
  maintenance(survivor, 'resume');
  paused = false;
  promoted = await waitUntil('surviving replica promotion and both real entrances', async () => {
    try {
      const observed = await sample(asset.id);
      const route = observed.metrics.routing;
      return observed.metrics.patroni?.leaderCount === 1 && route?.status === 'ok' && route.readStatus === 'ok' &&
        observed.metrics.patroni?.members?.some(item => item.name === survivor &&
          item.role === 'leader' && item.state === 'running') ? observed : null;
    } catch { return false; }
  }, 90000);
  assert.equal(promoted.metrics.routing.probe.role, 'primary');
  assert.equal(promoted.metrics.routing.readProbe.role, 'standby');
  assert.notEqual(promoted.metrics.routing.probe.backend, baseline.metrics.routing.probe.backend);
  assert.ok(promoted.metrics.patroni.history.entries.length > 0, 'actual DCS timeline history missing');
  assert.ok(promoted.metrics.patroni.nodeState.timeline > baseline.metrics.patroni.nodeState.timeline);
  docker('start', leaderContainer);
  stopped = false;
  recovery = await waitUntil('old leader rejoin as streaming replica', async () => {
    try {
      const observed = await sample(asset.id);
      return observed.status === 'ok' && observed.metrics.patroni?.leaderCount === 1 &&
        observed.metrics.patroni.members.length === 3 &&
        observed.metrics.patroni?.members?.some(item => item.name === leader &&
          ['replica', 'sync_standby'].includes(item.role) && item.state === 'streaming' &&
          Number.isFinite(item.replayLagBytes ?? item.lagBytes)) ? observed : null;
    } catch { return null; }
  }, 180000);
  console.log(JSON.stringify({ phase: 'rejoined', sampleId: recovery.id }));
} finally {
  if (paused) maintenance(survivor, 'resume');
  if (stopped) docker('start', leaderContainer);
  if (ruleChanged) await api('/api/alert-rules/patroni_leader_invalid', 'PATCH', { durationSeconds: 30 });
}

const events = (await api('/api/events')).body.events.filter(item => item.assetId === asset.id);
assert.equal(events.length, 1, 'one election must remain one availability incident');
const eventId = events[0].id;
const detail = (await api(`/api/events/${eventId}`)).body;
assert.equal(detail.event.status, 'resolved');
assert.equal(detail.event.eventGroup, 'availability');
assert.ok(detail.event.evidence.some(item => item.kind === 'alert_firing' &&
  item.payload?.ruleId === 'patroni_leader_invalid'));
assert.ok(detail.event.evidence.some(item => item.kind === 'alert_recovered' &&
  item.payload?.ruleId === 'patroni_leader_invalid'));
const closure = detail.event.evidence.find(item => item.kind === 'collection_recovered');
assert.ok(closure?.payload?.sampleId, 'availability event lacks its closing sample');
const closureSampleId = closure.payload.sampleId;
const closureSample = (await api(`/api/assets/${asset.id}/samples/${closureSampleId}`)).body.sample;
assert.equal(closureSample.status, 'ok');
assert.equal(closureSample.metrics.patroni?.leaderCount, 1);
assert.ok(detail.event.evidence.some(item => item.kind === 'alert_recovered' &&
  item.payload?.ruleId === 'pg_unavailable' && item.payload?.sampleId === closureSampleId));
assert.ok(detail.event.evidence.some(item => item.kind === 'patroni_state'));
const candidate = detail.analysis.candidates.find(item => item.code === 'patroni_leader_anomaly');
assert.ok(candidate, 'Patroni topology root-cause candidate missing');
assert.ok(candidate.supports.includes(`sample:${noLeaderSample.id}`));
assert.ok(detail.analysis.graph.nodes.some(item => item.id === `sample:${noLeaderSample.id}`));
assert.ok(detail.analysis.graph.nodes.some(item => item.id === `sample:${closureSampleId}`));
const reportResponse = await fetch(`${base}/api/events/${eventId}/report?format=md`,
  { headers: { Cookie: cookie }, signal: AbortSignal.timeout(20000) });
assert.equal(reportResponse.status, 200);
const report = await reportResponse.text();
assert.ok(report.includes(eventId) && report.includes('patroni_leader_anomaly') &&
  report.includes(noLeaderSample.id));

const query = new URLSearchParams({from: new Date(Date.parse(baseline.collectedAt) - 1000).toISOString(),
  to: new Date(Date.now() + 1000).toISOString()});
const history = (await api(`/api/assets/${asset.id}/replication?${query}`)).body;
assert.ok(history.topology.some(row => row.id === baseline.id && row.routes?.probe?.role === 'primary' &&
  row.routes?.readProbe?.role === 'standby'));
assert.ok(history.topology.some(row => row.id === promoted.id && row.routes?.probe?.backend === promoted.metrics.routing.probe.backend));
const proof = { detail, history, baseline, promoted, recovery, asset };
const browserProof = JSON.stringify(proof);
for (const value of Object.values(credentials)) {
  if (typeof value === 'string' && value.length >= 12) assert.ok(!browserProof.includes(value), 'drill credential leaked');
}
writeFileSync(resultPath.replace(/\.json$/, '.browser.json'), browserProof);
const result = { eventId, assetId: asset.id, targetPGMajor: Number(major), leader, survivor, noLeaderSampleId: noLeaderSample.id,
  unavailableSampleId: unavailableSample.id, recoverySampleId: closureSampleId,
  baselineSampleId: baseline.id, promotedSampleId: promoted.id, rejoinSampleId: recovery.id, status: detail.event.status,
  writeBefore: baseline.metrics.routing.probe.backend, writeAfter: promoted.metrics.routing.probe.backend,
  readAfter: promoted.metrics.routing.readProbe.backend,
  observationGapSeconds: (Date.parse(promoted.collectedAt) - Date.parse(baseline.collectedAt)) / 1000,
  oldPrimaryReplayLag: recovery.metrics.patroni.members.find(item => item.name === leader).replayLagBytes ??
    recovery.metrics.patroni.members.find(item => item.name === leader).lagBytes,
  candidate: candidate.code, storage: 'postgresql18' };
writeFileSync(resultPath, JSON.stringify(result, null, 2));
console.log(JSON.stringify(result, null, 2));
