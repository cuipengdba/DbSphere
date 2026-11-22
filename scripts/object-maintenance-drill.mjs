#!/usr/bin/env node

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';

const [base, credentialsPath, container, host, output] = process.argv.slice(2);
if (!base || !credentialsPath || !container || !host || !output) {
  throw new Error('Usage: object-maintenance-drill.mjs BASE CREDENTIALS FIXTURE_CONTAINER FIXTURE_HOST RESULT');
}
const secret = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const database = 'dbsphere_object_drill';
const monitor = 'dbsphere_object_monitor';
let cookie = '';
let csrf = '';

function sql(input, db = database) {
  try {
    return execFileSync('docker', ['exec', '-i', container, 'psql', '-U', 'postgres', '-d', db,
      '-v', 'ON_ERROR_STOP=1', '-Atq'], {input: input + '\n', encoding: 'utf8', timeout: 120000,
      stdio: ['pipe', 'pipe', 'pipe']}).trim();
  } catch (err) {
    let message = String(err.stderr || err.message);
    for (const value of Object.values(secret)) message = message.replaceAll(value, '[redacted]');
    throw new Error('Isolated maintenance SQL failed: ' + message);
  }
}
async function api(path, method = 'GET', data) {
  const response = await fetch(base + path, {method, headers: {
    ...(cookie ? {Cookie: cookie} : {}), ...(csrf && method !== 'GET' ? {'X-CSRF-Token': csrf} : {}),
    ...(data ? {'Content-Type': 'application/json'} : {})
  }, body: data ? JSON.stringify(data) : undefined, signal: AbortSignal.timeout(45000)});
  const body = await response.json();
  assert.ok(response.ok, method + ' ' + path + ': ' + response.status + ' ' + JSON.stringify(body));
  if (path === '/api/login') {
    cookie = response.headers.get('set-cookie')?.split(';')[0] || '';
    csrf = body.csrf;
    assert.ok(cookie && csrf);
  }
  return body;
}
async function sample(asset) {
  const checked = await api('/api/assets/' + asset.id + '/check', 'POST');
  assert.equal(checked.result.status, 'ok');
  return (await api('/api/assets/' + asset.id + '/samples/' + checked.result.sampleId)).sample;
}

assert.equal((await api('/readyz')).storage, 'postgresql18');
sql('CREATE DATABASE ' + database + ';\nCREATE ROLE ' + monitor + " LOGIN PASSWORD '" + secret.monitorPassword + "';\n" +
  'GRANT pg_monitor TO ' + monitor + ';', 'postgres');
sql([
  'CREATE SCHEMA maintenance;',
  'CREATE TABLE maintenance.ledger(id int PRIMARY KEY, payload text) WITH (autovacuum_enabled=false, ' +
    'autovacuum_freeze_max_age=100000, autovacuum_vacuum_threshold=100, autovacuum_vacuum_scale_factor=0.1, ' +
    'autovacuum_vacuum_max_threshold=2000, autovacuum_analyze_threshold=50, autovacuum_analyze_scale_factor=0.05);',
  "INSERT INTO maintenance.ledger SELECT n, repeat(md5(n::text),8) FROM generate_series(1,36000) n;",
  'CREATE TABLE maintenance.events(region int, payload text) PARTITION BY RANGE(region);',
  'CREATE TABLE maintenance.events_hot PARTITION OF maintenance.events FOR VALUES FROM (0) TO (10);',
  'CREATE TABLE maintenance.events_cold PARTITION OF maintenance.events FOR VALUES FROM (10) TO (20);',
  "INSERT INTO maintenance.events SELECT 1,'hot' FROM generate_series(1,18000);",
  "INSERT INTO maintenance.events SELECT 11,'cold' FROM generate_series(1,60);",
  'VACUUM (FREEZE, ANALYZE) maintenance.ledger;',
  'VACUUM (ANALYZE) maintenance.events_hot;',
  'VACUUM (ANALYZE) maintenance.events_cold;',
  'ANALYZE maintenance.events;',
  'SELECT pg_stat_force_next_flush();'
].join('\n'));

await api('/api/login', 'POST', {password: secret.adminPassword});
await api('/api/alert-rules/pg_vacuum_backlog', 'PATCH', {durationSeconds: 0});
const asset = (await api('/api/assets', 'POST', {name: '对象维护与偏斜分区', clusterName: 'PG18 对象维护演练',
  host, port: 5432, database, username: monitor, password: secret.monitorPassword, sslMode: 'disable', runtime: 'linux'})).asset;
const tables = row => row.metrics.diagnostics.tableInventory.data.objects;
const indexes = row => row.metrics.diagnostics.indexInventory.data.objects;
const ledger = row => tables(row).find(item => item.schema === 'maintenance' && item.name === 'ledger');
const initial = await sample(asset);
const target = ledger(initial);
assert.ok(target.heapBytes >= 5 * 1024 * 1024);
assert.equal(target.vacuumTriggerRows, 2000);
assert.equal(target.xidLimit, 100000);
assert.equal(target.autovacuumEnabled, false);
assert.equal(target.deadRows, 0);

sql("INSERT INTO maintenance.ledger SELECT n,repeat(md5(n::text),16) FROM generate_series(36001,45000) n;\n" +
  "UPDATE maintenance.ledger SET payload=repeat(md5((id+10)::text),12) WHERE id<=28000;\n" +
  'SELECT pg_stat_force_next_flush();\n' + 'SELECT pg_current_xact_id();\n'.repeat(1024));
assert.throws(() => sql('CREATE UNIQUE INDEX CONCURRENTLY ledger_duplicate_invalid ON maintenance.ledger ((id % 2));'),
  /could not create unique index|duplicate key/);
await delay(1200);
const pressure = await sample(asset);
const busy = ledger(pressure);
assert.equal(busy.oid, target.oid);
assert.ok(busy.deadRows >= 20000 && busy.deadHeapBytesEstimate > 0 && busy.bytes > target.bytes);
assert.ok(busy.xidAge > target.xidAge && busy.xidAge > 1000 && Number.isFinite(busy.mxidAge));
assert.ok(busy.modifiedSinceAnalyze > busy.analyzeTriggerRows);
const partition = tables(pressure).find(item => item.name === 'events');
const hot = tables(pressure).find(item => item.name === 'events_hot');
const cold = tables(pressure).find(item => item.name === 'events_cold');
assert.equal(partition.directPartitions, 2);
assert.equal(partition.heapBytes, null);
assert.equal(hot.parentOid, partition.oid);
assert.equal(cold.parentOid, partition.oid);
assert.ok(hot.liveRows > cold.liveRows * 100);
const invalid = indexes(pressure).find(item => item.name === 'ledger_duplicate_invalid');
assert.equal(invalid.valid, false);
assert.equal(invalid.method, 'btree');
assert.ok(indexes(pressure).some(item => item.tableOid === busy.oid && item.primary && item.valid));
const databaseOid = pressure.metrics.diagnostics.databaseInventory.data.databases.find(row => row.connected).oid;
const maintenanceEvents = async () => (await api('/api/events')).events.filter(row =>
  row.assetId === asset.id && row.eventGroup === 'maintenance');
const events = await maintenanceEvents();
assert.equal(events.length, 1);
const active = events[0];
assert.equal(active.status, 'open');
console.log(JSON.stringify({phase: 'maintenance-pressure', event: active.id, tableOid: busy.oid,
  deadRows: busy.deadRows, heapEstimate: busy.deadHeapBytesEstimate, xid: busy.xidAge}));

sql('VACUUM (FREEZE, ANALYZE) maintenance.ledger;\nSELECT pg_stat_force_next_flush();');
await delay(1200);
const recovered = await sample(asset);
const clean = ledger(recovered);
assert.equal(clean.oid, busy.oid);
assert.equal(clean.deadRows, 0);
assert.equal(clean.deadHeapBytesEstimate, 0);
assert.ok(clean.xidAge < busy.xidAge && clean.modifiedSinceAnalyze < busy.modifiedSinceAnalyze);
const resolved = await maintenanceEvents();
assert.equal(resolved.length, 1);
assert.equal(resolved[0].id, active.id);
assert.equal(resolved[0].status, 'resolved');
const detail = await api('/api/events/' + active.id);
const candidate = detail.analysis.candidates.find(row => row.code === 'vacuum_backlog');
assert.ok(candidate?.supports.includes('sample:' + pressure.id));
assert.ok(detail.event.evidence.some(row => row.kind === 'alert_firing' &&
  row.payload?.sampleId === pressure.id && row.payload.detail?.tableOid === busy.oid &&
  row.payload.detail?.databaseOid === databaseOid));
assert.ok(detail.event.evidence.some(row => row.kind === 'alert_recovered' && row.payload?.sampleId === recovered.id));

const query = new URLSearchParams({from: new Date(Date.parse(initial.collectedAt) - 1000).toISOString(),
  to: new Date(Date.now() + 1000).toISOString(), bucketMinutes: '1', dbOid: databaseOid, objectOid: busy.oid, objectKind: 'table'});
const catalog = await api('/api/assets/' + asset.id + '/catalog?' + query);
for (const kind of ['vacuum', 'analyze']) {
  const frame = catalog.maintenance.find(row => row.kind === kind && row.beforeId === pressure.id && row.afterId === recovered.id);
  assert.ok(frame && frame.automatic === false && frame.before.deadRows > frame.after.deadRows, kind);
}
const result = {eventId: active.id, assetId: asset.id, databaseOid, tableOid: busy.oid, invalidIndexOid: invalid.oid, rootOid: partition.oid,
  hotRows: hot.liveRows, coldRows: cold.liveRows, initialBytes: target.bytes, pressureBytes: busy.bytes,
  deadRowsBefore: busy.deadRows, deadRowsAfter: clean.deadRows, estimatedBytesBefore: busy.deadHeapBytesEstimate,
  estimatedBytesAfter: clean.deadHeapBytesEstimate, xidBefore: busy.xidAge, xidAfter: clean.xidAge,
  baselineSampleId: initial.id, pressureSampleId: pressure.id, recoveredSampleId: recovered.id,
  maintenance: catalog.maintenance, status: detail.event.status};
const encoded = JSON.stringify(result, null, 2);
for (const value of Object.values(secret)) assert.ok(!encoded.includes(value), 'Fixture credential escaped');
writeFileSync(output, encoded);
console.log(encoded);
