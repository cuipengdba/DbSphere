#!/usr/bin/env node

// Real PG18 autovacuum configuration fault in the isolated replication fixture.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';

const [baseURL, credentialsPath] = process.argv.slice(2);
if (!baseURL || !credentialsPath) {
  console.error('usage: node scripts/autovacuum-drill-acceptance.mjs BASE_URL CREDENTIALS_PATH');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const credentials = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const primary = 'dbsphere-replication-drill-maintenance-1';
let cookie = '';
let csrf = '';
let changed = false;

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
  const settings = row.metrics.diagnostics?.settings;
  assert.equal(settings?.status, 'ok');
  const autovacuum = settings.data.find(setting => setting.name === 'autovacuum');
  assert.ok(autovacuum);
  return { id: result.sampleId, metrics: row.metrics, autovacuum: autovacuum.value };
}

assert.equal((await api('/readyz')).body.storage, 'postgresql18');
if (psql('SHOW autovacuum') !== 'on') {
  psql("ALTER SYSTEM SET autovacuum = 'on'");
  assert.equal(psql('SELECT pg_reload_conf()'), 't');
}
assert.equal(psql('SHOW autovacuum'), 'on');
const login = await api('/api/login', 'POST', { password: credentials.adminPassword });
cookie = login.response.headers.get('set-cookie')?.split(';')[0] || '';
csrf = login.body.csrf;
assert.ok(cookie && csrf);
const asset = (await api('/api/assets', 'POST', {
  name: `PG18 autovacuum drill ${Date.now()}`, host: 'maintenance', port: 5432,
  database: 'postgres', username: 'dbsphere_monitor', password: credentials.monitorPassword,
  sslMode: 'disable', runtime: 'linux'
})).body.asset;
assert.ok(asset.id);
const baseline = await sample(asset.id);
assert.equal(baseline.autovacuum, 'on');
assert.equal(baseline.metrics.monitor_role, true);
assert.equal(baseline.metrics.privileged_role, false);

try {
  psql("ALTER SYSTEM SET autovacuum = 'off'");
  changed = true;
  assert.equal(psql('SELECT pg_reload_conf()'), 't');
  assert.equal(psql('SHOW autovacuum'), 'off');
  const fault = await sample(asset.id);
  assert.equal(fault.autovacuum, 'off');
  const events = (await api('/api/events')).body.events.filter(row => row.assetId === asset.id);
  assert.equal(events.length, 1, 'expected one maintenance incident');
  const eventId = events[0].id;
  assert.equal(events[0].status, 'open');
  const detail = (await api(`/api/events/${eventId}`)).body;
  assert.equal(detail.event.eventGroup, 'maintenance');
  assert.ok(detail.event.evidence.some(row => row.kind === 'alert_firing' &&
    row.payload?.ruleId === 'pg_autovacuum_disabled' && row.payload?.sampleId === fault.id));
  const candidate = detail.analysis.candidates.find(row => row.code === 'autovacuum_disabled');
  assert.ok(candidate?.supports.includes(`sample:${fault.id}`));
  assert.equal(candidate.confidence, 'low', 'a disabled setting alone does not prove query impact');
  const reportResponse = await fetch(`${base}/api/events/${eventId}/report?format=md`,
    { headers: { Cookie: cookie }, signal: AbortSignal.timeout(20000) });
  assert.equal(reportResponse.status, 200);
  const report = await reportResponse.text();
  assert.ok(report.includes(eventId) && report.includes('autovacuum_disabled') && report.includes(fault.id));

  psql("ALTER SYSTEM SET autovacuum = 'on'");
  assert.equal(psql('SELECT pg_reload_conf()'), 't');
  changed = false;
  assert.equal(psql('SHOW autovacuum'), 'on');
  const recovery = await sample(asset.id);
  assert.equal(recovery.autovacuum, 'on');
  const final = (await api(`/api/events/${eventId}`)).body;
  assert.equal(final.event.status, 'resolved');
  assert.ok(final.event.evidence.some(row => row.kind === 'alert_recovered' &&
    row.payload?.ruleId === 'pg_autovacuum_disabled' && row.payload?.sampleId === recovery.id));
  console.log(JSON.stringify({ eventId, assetId: asset.id, faultSampleId: fault.id,
    recoverySampleId: recovery.id, status: final.event.status,
    candidate: candidate.code, confidence: candidate.confidence, storage: 'postgresql18' }, null, 2));
} finally {
  if (changed) {
    try {
      psql("ALTER SYSTEM SET autovacuum = 'on'");
      psql('SELECT pg_reload_conf()');
      await sample(asset.id);
    } catch (error) {
      console.error(`Could not restore isolated autovacuum setting automatically: ${error.message}`);
    }
  }
}
