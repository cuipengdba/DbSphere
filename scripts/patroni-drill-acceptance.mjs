#!/usr/bin/env node

// Real two-node Patroni election against the Go API and an independent PG18 store.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';

const [baseURL, credentialsPath, envPath, resultPath] = process.argv.slice(2);
if (!baseURL || !credentialsPath || !envPath || !resultPath) {
  console.error('usage: node scripts/patroni-drill-acceptance.mjs BASE_URL CREDENTIALS_PATH ENV_PATH RESULT_PATH');
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
  return execFileSync('docker', args, { encoding: 'utf8', timeout: 30000 }).trim();
}
async function cluster() {
  const response = await fetch('http://127.0.0.1:18018/cluster', { signal: AbortSignal.timeout(10000) });
  assert.equal(response.status, 200);
  return response.json();
}
async function sample(assetId) {
  const result = (await api(`/api/assets/${assetId}/check`, 'POST')).body.result;
  const row = (await api(`/api/assets/${assetId}/samples/${result.sampleId}`)).body.sample;
  return { id: result.sampleId, status: result.status, metrics: row.metrics };
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
await waitUntil('one leader and one streaming replica', async () => {
  const members = (await cluster()).members;
  return members.length === 2 && members.some(item => item.role === 'leader' && item.state === 'running') &&
    members.some(item => item.role === 'replica' && item.state === 'streaming');
});
const members = (await cluster()).members;
const leader = members.find(item => item.role === 'leader').name;
const survivor = members.find(item => item.name !== leader).name;
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
  name: `Patroni election ${Date.now()}`, host: leader, port: 5432, database: 'postgres',
  username: 'dbsphere_monitor', password: credentials.monitorPassword,
  sslMode: 'disable', runtime: 'linux', patroniUrl: `http://${survivor}:8008`
})).body.asset;
assert.ok(asset.id && asset.patroniConfigured);
const baseline = await sample(asset.id);
assert.equal(baseline.status, 'ok');
assert.equal(baseline.metrics.patroni?.leaderCount, 1);
assert.equal(baseline.metrics.patroni?.replicaCount, 1);
assert.equal(baseline.metrics.monitor_role, true);
assert.equal(baseline.metrics.privileged_role, false);
assert.ok(!JSON.stringify(baseline).includes(credentials.monitorPassword));

const leaderContainer = container(leader);
let stopped = false;
let ruleChanged = false;
let noLeaderSample;
let unavailableSample;
let recovery;
try {
  await api('/api/alert-rules/patroni_leader_invalid', 'PATCH', { durationSeconds: 0 });
  ruleChanged = true;
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
      noLeaderSample ??= observed;
    }
    if (observed.status === 'error' && observed.metrics.collector?.pgStatus === 'availability') {
      unavailableSample ??= observed;
    }
    if (noLeaderSample && unavailableSample && observed.metrics.patroni?.leaderCount === 1) break;
    await delay(2500);
  }
  assert.ok(noLeaderSample, 'real leader election did not yield a no-leader sample');
  assert.ok(unavailableSample, 'stopped leader did not yield a PostgreSQL availability sample');
  docker('start', leaderContainer);
  stopped = false;
  recovery = await waitUntil('old leader rejoin as streaming replica', async () => {
    try {
      const observed = await sample(asset.id);
      return observed.status === 'ok' && observed.metrics.patroni?.leaderCount === 1 &&
        observed.metrics.patroni?.members?.some(item => item.name === leader &&
          item.role === 'replica' && item.state === 'streaming') ? observed : null;
    } catch { return null; }
  }, 180000);
} finally {
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
  item.payload?.sampleId === recovery.id));
assert.ok(detail.event.evidence.some(item => item.kind === 'patroni_state'));
const candidate = detail.analysis.candidates.find(item => item.code === 'patroni_leader_anomaly');
assert.ok(candidate, 'Patroni topology root-cause candidate missing');
assert.ok(candidate.supports.includes(`sample:${noLeaderSample.id}`));
assert.ok(detail.analysis.graph.nodes.some(item => item.id === `sample:${noLeaderSample.id}`));
assert.ok(detail.analysis.graph.nodes.some(item => item.id === `sample:${recovery.id}`));
const reportResponse = await fetch(`${base}/api/events/${eventId}/report?format=md`,
  { headers: { Cookie: cookie }, signal: AbortSignal.timeout(20000) });
assert.equal(reportResponse.status, 200);
const report = await reportResponse.text();
assert.ok(report.includes(eventId) && report.includes('patroni_leader_anomaly') &&
  report.includes(noLeaderSample.id));

const result = { eventId, assetId: asset.id, leader, survivor, noLeaderSampleId: noLeaderSample.id,
  unavailableSampleId: unavailableSample.id, recoverySampleId: recovery.id, status: detail.event.status,
  candidate: candidate.code, storage: 'postgresql18' };
writeFileSync(resultPath, JSON.stringify(result, null, 2));
console.log(JSON.stringify(result, null, 2));
