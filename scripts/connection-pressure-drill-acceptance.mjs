#!/usr/bin/env node

// Real PG18 connection pressure, using idle pgbench clients in an isolated DB.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';

const [baseURL, credentialsPath] = process.argv.slice(2);
if (!baseURL || !credentialsPath) {
  console.error('usage: node scripts/connection-pressure-drill-acceptance.mjs BASE_URL CREDENTIALS_PATH');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const credentials = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const fixture = 'dbsphere-replication-drill-maintenance-1';
let cookie = '';
let csrf = '';
let clientsStarted = false;

function docker(...args) {
  return execFileSync('docker', args, { encoding: 'utf8', timeout: 120000 }).trim();
}
function psql(sql) {
  return docker('exec', fixture, 'psql', '-U', 'postgres', '-Atc', sql);
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
  assert.equal(result.status, 'ok', result.error || 'PostgreSQL unavailable under test load');
  const { sample: row } = (await api(`/api/assets/${assetId}/samples/${result.sampleId}`)).body;
  assert.equal(row.status, 'ok');
  return { id: result.sampleId, metrics: row.metrics };
}
async function until(label, check, timeoutMs = 60000) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const value = await check();
    if (value) return value;
    await delay(2000);
  }
  throw new Error(`timed out waiting for ${label}`);
}
function activeClients() {
  return Number(psql("SELECT count(*) FROM pg_stat_activity WHERE application_name='dbsphere-connection-drill'"));
}
function stopClients() {
  try { docker('exec', fixture, 'sh', '-c', 'kill $(pidof pgbench)'); }
  catch (error) { if (activeClients() > 0) throw error; }
}

assert.equal((await api('/readyz')).body.storage, 'postgresql18');
assert.equal(Number(psql('SHOW max_connections')), 30);
const login = await api('/api/login', 'POST', { password: credentials.adminPassword });
cookie = login.response.headers.get('set-cookie')?.split(';')[0] || '';
csrf = login.body.csrf;
assert.ok(cookie && csrf);
const asset = (await api('/api/assets', 'POST', {
  name: `PG18 connection pressure ${Date.now()}`, host: 'maintenance', port: 5432,
  database: 'postgres', username: 'dbsphere_monitor', password: credentials.monitorPassword,
  sslMode: 'disable', runtime: 'linux'
})).body.asset;
assert.ok(asset.id);
const baseline = await sample(asset.id);
assert.equal(baseline.metrics.max_connections, 30);
assert.equal(baseline.metrics.monitor_role, true);
assert.equal(baseline.metrics.privileged_role, false);

let eventId = '';
try {
  execFileSync('docker', ['exec', '-i', fixture, 'sh', '-c', 'cat > /tmp/dbsphere-connection-hold.sql'],
    { input: '\\sleep 240 s\nSELECT 1;\n', encoding: 'utf8', timeout: 30000 });
  docker('exec', '-d', '-e', 'PGAPPNAME=dbsphere-connection-drill', fixture,
    'pgbench', '-n', '-c', '25', '-j', '5', '-T', '300', '-f', '/tmp/dbsphere-connection-hold.sql',
    '-U', 'postgres', 'postgres');
  clientsStarted = true;
  await until('25 idle fixture connections', () => activeClients() >= 25);
  const first = await sample(asset.id);
  const utilization = first.metrics.connections / first.metrics.max_connections * 100;
  assert.ok(utilization >= 85, `observed utilization ${utilization}% below rule threshold`);
  await delay(125000); // Default pg_connections_high rule requires 120 seconds.
  const fault = await sample(asset.id);
  assert.ok(fault.metrics.connections / fault.metrics.max_connections * 100 >= 85);
  const events = (await api('/api/events')).body.events.filter(row => row.assetId === asset.id);
  assert.equal(events.length, 1, 'expected one connection-capacity incident');
  eventId = events[0].id;
  assert.equal(events[0].status, 'open');
  const detail = (await api(`/api/events/${eventId}`)).body;
  assert.equal(detail.event.eventGroup, 'capacity');
  assert.ok(detail.event.evidence.some(row => row.kind === 'alert_firing' &&
    row.payload?.ruleId === 'pg_connections_high' && row.payload?.sampleId === fault.id));
  const candidate = detail.analysis.candidates.find(row => row.code === 'connection_pressure');
  assert.ok(candidate?.supports.includes(`sample:${fault.id}`));
  assert.equal(candidate.state, 'hypothesis', 'high utilization alone must not prove a root cause');
  assert.ok(candidate.missing?.length > 0 && candidate.limitation?.includes('尚未证明'),
    'capacity candidate must expose missing connection-refusal evidence');
  const reportResponse = await fetch(`${base}/api/events/${eventId}/report?format=md`,
    { headers: { Cookie: cookie }, signal: AbortSignal.timeout(20000) });
  assert.equal(reportResponse.status, 200);
  const report = await reportResponse.text();
  assert.ok(report.includes(eventId) && report.includes('connection_pressure') && report.includes(fault.id));

  stopClients();
  clientsStarted = false;
  await until('fixture client disconnect', () => activeClients() === 0);
  const recovery = await sample(asset.id);
  const final = (await api(`/api/events/${eventId}`)).body;
  assert.equal(final.event.status, 'resolved');
  assert.ok(final.event.evidence.some(row => row.kind === 'alert_recovered' &&
    row.payload?.ruleId === 'pg_connections_high' && row.payload?.sampleId === recovery.id));
  console.log(JSON.stringify({ eventId, assetId: asset.id, faultSampleId: fault.id,
    recoverySampleId: recovery.id, connectionsAtFault: fault.metrics.connections,
    maxConnections: fault.metrics.max_connections, connectionsAtRecovery: recovery.metrics.connections,
    status: final.event.status, candidate: candidate.code, storage: 'postgresql18' }, null, 2));
} finally {
  if (clientsStarted) {
    try {
      stopClients();
      if (eventId) await sample(asset.id);
    } catch (error) {
      console.error(`Could not stop isolated pgbench clients automatically: ${error.message}`);
    }
  }
}
