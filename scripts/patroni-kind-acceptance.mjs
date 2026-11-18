#!/usr/bin/env node

// Real Patroni election inside the isolated dbsphere-smoke kind namespace.
// KUBECONFIG must point to the local test-cluster admin config; DbSphere itself
// only reads PostgreSQL, Patroni REST and its in-cluster read-only K8S API.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';

const [baseURL, credentialsPath, adminPasswordPath, resultPath, major = '14'] = process.argv.slice(2);
if (!baseURL || !credentialsPath || !adminPasswordPath || !resultPath || !process.env.KUBECONFIG) {
  console.error('usage: KUBECONFIG=... node scripts/patroni-kind-acceptance.mjs BASE_URL CREDENTIALS ADMIN_PASSWORD RESULT [PG_MAJOR]');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const credentials = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const adminPassword = readFileSync(adminPasswordPath, 'utf8').trim();
let cookie = '';
let csrf = '';

function kubectl(...args) {
  return execFileSync('kubectl', ['-n', 'dbsphere-smoke', ...args],
    { encoding: 'utf8', timeout: 60000 }).trim();
}
function cluster() {
  return JSON.parse(kubectl('exec', 'deploy/patroni2', '--', 'patronictl', '-c',
    '/tmp/dbsphere-patroni.yml', 'list', 'dbsphere-ci-patroni', '-f', 'json'));
}
function maintenance(service, action) {
  return kubectl('exec', `deploy/${service}`, '--', 'patronictl', '-c',
    '/tmp/dbsphere-patroni.yml', action, 'dbsphere-ci-patroni', ...(action === 'pause' ? ['--wait'] : []));
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
  const { sample: row } = (await api(`/api/assets/${assetId}/samples/${result.sampleId}`)).body;
  return { id: result.sampleId, status: row.status, metrics: row.metrics };
}
async function until(label, check, timeoutMs = 150000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    try {
      const result = await check();
      if (result) return result;
    } catch (error) {
      console.error(`${label}: ${error.message}`);
    }
    await delay(3000);
  }
  throw new Error(`timed out waiting for ${label}`);
}

assert.equal((await api('/readyz')).body.storage, 'postgresql18');
const healthy = await until('Patroni leader and replica', () => {
  const members = cluster();
  return members.length === 2 && members.some(row => row.Role === 'Leader' && row.State === 'running') &&
    members.some(row => row.Role === 'Replica' && row.State === 'streaming') ? members : null;
});
const leader = healthy.find(row => row.Role === 'Leader').Member;
const survivor = healthy.find(row => row.Member !== leader).Member;
assert.ok(['patroni1', 'patroni2'].includes(leader));

// The fixture creates a dedicated pg_monitor login; no DbSphere write API is used.
const escaped = credentials.monitorPassword.replaceAll("'", "''");
execFileSync('kubectl', ['-n', 'dbsphere-smoke', 'exec', '-i', `deploy/${leader}`,
  '--', 'psql', '-U', 'postgres', '-v', 'ON_ERROR_STOP=1'], {
  input: `DO $test$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'dbsphere_monitor') THEN
      CREATE ROLE dbsphere_monitor LOGIN PASSWORD '${escaped}';
    ELSE
      ALTER ROLE dbsphere_monitor PASSWORD '${escaped}';
    END IF;
  END $test$;
  GRANT pg_monitor TO dbsphere_monitor;\n`,
  encoding: 'utf8', timeout: 30000
});

const login = await api('/api/login', 'POST', { password: adminPassword });
cookie = login.response.headers.get('set-cookie')?.split(';')[0] || '';
csrf = login.body.csrf;
assert.ok(cookie && csrf);
const asset = (await api('/api/assets', 'POST', {
  name: `kind Patroni election ${Date.now()}`,
  host: leader, port: 5432, database: 'postgres', username: 'dbsphere_monitor',
  password: credentials.monitorPassword, sslMode: 'disable', runtime: 'k8s',
  k8sCluster: 'kind-dbsphere-test', namespace: 'dbsphere-smoke', workload: leader,
  patroniUrl: `http://${survivor}:8008`
})).body.asset;
assert.ok(asset.id && asset.patroniConfigured);
const baseline = await sample(asset.id);
assert.equal(baseline.status, 'ok');
assert.equal(Math.floor(baseline.metrics.version_num / 10000), Number(major));
assert.equal(baseline.metrics.patroni?.leaderCount, 1);
assert.equal(baseline.metrics.patroni?.replicaCount, 1);
assert.equal(baseline.metrics.k8s?.status, 'ok');
assert.equal(baseline.metrics.monitor_role, true);
assert.equal(baseline.metrics.privileged_role, false);
console.log(JSON.stringify({ phase: 'baseline', leader, survivor, sampleId: baseline.id }));

let paused = false;
let scaledDown = false;
let ruleChanged = false;
let noLeaderSample;
let unavailableSample;
let recovery;
try {
  await api('/api/alert-rules/patroni_leader_invalid', 'PATCH', { durationSeconds: 0 });
  ruleChanged = true;
  maintenance(survivor, 'pause');
  paused = true;
  kubectl('scale', `deploy/${leader}`, '--replicas=0');
  scaledDown = true;
  await until('old leader pod removal', () =>
    Number(kubectl('get', `deploy/${leader}`, '-o', 'jsonpath={.status.readyReplicas}')) === 0);
  await until('PG unavailable and no Patroni leader', async () => {
    const observed = await sample(asset.id);
    if (observed.metrics.patroni?.status === 'ok' && observed.metrics.patroni.leaderCount === 0) {
      noLeaderSample ||= observed;
    }
    if (observed.status === 'error' && observed.metrics.collector?.pgStatus === 'availability') {
      unavailableSample ||= observed;
    }
    return noLeaderSample && unavailableSample;
  }, 150000);
  console.log(JSON.stringify({ phase: 'no-leader', sampleId: noLeaderSample.id,
    unavailableSampleId: unavailableSample.id }));
  maintenance(survivor, 'resume');
  paused = false;
  await until('surviving Patroni promotion', async () => {
    const observed = await sample(asset.id);
    return observed.metrics.patroni?.members?.some(row => row.name === survivor &&
      row.role === 'leader' && row.state === 'running');
  }, 120000);
  kubectl('scale', `deploy/${leader}`, '--replicas=1');
  scaledDown = false;
  recovery = await until('old member rejoin and PostgreSQL recovery', async () => {
    const observed = await sample(asset.id);
    return observed.status === 'ok' && observed.metrics.patroni?.leaderCount === 1 &&
      observed.metrics.patroni?.members?.some(row => row.name === leader &&
        row.role === 'replica' && row.state === 'streaming') &&
      observed.metrics.k8s?.status === 'ok' ? observed : null;
  }, 180000);
  console.log(JSON.stringify({ phase: 'rejoined', sampleId: recovery.id }));
} finally {
  if (paused) maintenance(survivor, 'resume');
  if (scaledDown) kubectl('scale', `deploy/${leader}`, '--replicas=1');
  if (ruleChanged) await api('/api/alert-rules/patroni_leader_invalid', 'PATCH', { durationSeconds: 30 });
}

const events = (await api('/api/events')).body.events.filter(row => row.assetId === asset.id);
const availability = events.filter(row => row.eventGroup === 'availability');
assert.equal(availability.length, 1, 'one election must remain one availability incident');
assert.ok(events.every(row => ['availability', 'telemetry'].includes(row.eventGroup)));
const eventId = availability[0].id;
const detail = (await api(`/api/events/${eventId}`)).body;
assert.equal(detail.event.status, 'resolved');
assert.equal(detail.event.eventGroup, 'availability');
assert.ok(detail.event.evidence.some(row => row.kind === 'patroni_state'));
assert.ok(detail.event.evidence.some(row => row.kind === 'k8s_state'),
  'Kubernetes mapping gap must be visible in the same availability incident');
assert.ok(detail.event.evidence.some(row => row.kind === 'alert_firing' &&
  row.payload?.ruleId === 'patroni_leader_invalid'));
const closing = detail.event.evidence.find(row => row.kind === 'collection_recovered');
assert.ok(closing?.payload?.sampleId);
const candidate = detail.analysis.candidates.find(row => row.code === 'patroni_leader_anomaly');
assert.ok(candidate?.supports.includes(`sample:${noLeaderSample.id}`));
assert.ok(detail.analysis.graph.nodes.some(row => row.id === `sample:${closing.payload.sampleId}`));
const telemetryEvents = events.filter(row => row.eventGroup === 'telemetry');
for (const telemetry of telemetryEvents) {
  assert.ok(detail.relatedEvents?.some(row => row.id === telemetry.id &&
    row.eventGroup === 'telemetry' && row.sharedSampleId === unavailableSample.id),
    'same-sample telemetry gap must link from the availability incident');
}
const reportResponse = await fetch(`${base}/api/events/${eventId}/report?format=md`,
  { headers: { Cookie: cookie }, signal: AbortSignal.timeout(20000) });
assert.equal(reportResponse.status, 200);
const report = await reportResponse.text();
assert.ok(report.includes(eventId) && report.includes('patroni_leader_anomaly') &&
  report.includes(noLeaderSample.id), 'report must preserve the availability evidence chain');
for (const telemetry of telemetryEvents) assert.ok(report.includes(telemetry.id));
const result = { eventId, assetId: asset.id, targetPGMajor: Number(major), leader, survivor,
  noLeaderSampleId: noLeaderSample.id, unavailableSampleId: unavailableSample.id,
  recoverySampleId: closing.payload.sampleId, rejoinSampleId: recovery.id,
  status: detail.event.status, candidate: candidate.code,
  k8sPod: recovery.metrics.k8s?.pod?.name,
  telemetryEventIds: telemetryEvents.map(row => row.id),
  storage: 'postgresql18' };
writeFileSync(resultPath, JSON.stringify(result, null, 2));
console.log(JSON.stringify(result, null, 2));
