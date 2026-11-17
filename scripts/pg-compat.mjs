import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import pg from 'pg';
import { createServer } from 'node:http';
import { randomBytes } from 'node:crypto';
import { collectPostgres } from '../src/postgres.js';
import { DEFAULT_INSPECTION_TEMPLATES, evaluateInspection } from '../src/inspections.js';
import { Store } from '../src/store.js';
import { analyzeEvent, eventSampleIds } from '../src/rca.js';
import { parseOtlpTraces } from '../src/otel.js';

const major = Number(process.env.DBSPHERE_TEST_PG_MAJOR);
if (!Number.isInteger(major)) throw new Error('Set DBSPHERE_TEST_PG_MAJOR');
const metrics = await collectPostgres({
  host: process.env.DBSPHERE_TEST_PG_HOST || '127.0.0.1',
  port: Number(process.env.DBSPHERE_TEST_PG_PORT || 15432),
  database_name: 'postgres',
  username: 'dbsphere_monitor',
  ssl_mode: 'disable'
}, process.env.DBSPHERE_TEST_PG_PASSWORD || 'dbsphere-monitor-test');
assert.equal(Math.floor(metrics.version_num / 10000), major);
assert.equal(metrics.pg14_eol, major === 14);
assert.equal(metrics.role, 'primary');
assert.equal(metrics.monitor_role, true);
assert.equal(metrics.privileged_role, false);
assert.ok(metrics.connections >= 1);
assert.ok(metrics.database_bytes > 0);
assert.ok(metrics.replicas !== null);
assert.equal(metrics.sqlPerformance.status, 'not_configured');
for (const name of ['activity', 'transactions', 'locks', 'lockChain', 'database', 'wal', 'archiver', 'checkpoints', 'vacuum', 'tables', 'slots', 'freeze', 'slotDetails', 'settings']) {
  assert.equal(metrics.diagnostics[name].status, 'ok', `${name}: ${metrics.diagnostics[name].reason}`);
}
const inspection = evaluateInspection(DEFAULT_INSPECTION_TEMPLATES.find(item => item.id === 'pg_daily'),
  { runtime: 'linux' }, { status: 'ok', collectedAt: new Date().toISOString(), metrics }, null);
assert.equal(inspection.findings.find(item => item.id === 'pg14_eol').status, major === 14 ? 'risk' : 'pass');
console.log(`PG${major} compatible: ${Object.keys(metrics.diagnostics).length} diagnostic sections available`);

const connection = {
  host: process.env.DBSPHERE_TEST_PG_HOST || '127.0.0.1',
  port: Number(process.env.DBSPHERE_TEST_PG_PORT || 15432),
  database: 'postgres', user: 'postgres', password: 'dbsphere-root-test',
  query_timeout: 12000
};
const blocker = new pg.Client(connection);
const waiter = new pg.Client(connection);
await blocker.connect();
await waiter.connect();
let waiting;
let requestServer;
let requestStart;
let dbStart;
let dbEnd;
let requestEnd;
try {
  const blockerPid = (await blocker.query('SELECT pg_backend_pid() AS pid')).rows[0].pid;
  const waitingPid = (await waiter.query('SELECT pg_backend_pid() AS pid')).rows[0].pid;
  await blocker.query('SELECT pg_advisory_lock(1742026)');
  requestServer = createServer(async (_req, res) => {
    requestStart = new Date();
    dbStart = new Date();
    try {
      await waiter.query('SELECT pg_advisory_lock(1742026)');
      dbEnd = new Date();
      res.writeHead(200);
      res.end('ok');
    } catch (error) {
      dbEnd = new Date();
      res.writeHead(500);
      res.end('query failed');
    } finally { requestEnd = new Date(); }
  });
  await new Promise(resolve => requestServer.listen(0, '127.0.0.1', resolve));
  waiting = fetch(`http://127.0.0.1:${requestServer.address().port}/checkout`);
  let found = false;
  let lockedSample = null;
  for (let attempt = 0; attempt < 10; attempt++) {
    await new Promise(resolve => setTimeout(resolve, 100));
    const sample = await collectPostgres({
      host: connection.host, port: connection.port, database_name: 'postgres',
      username: 'dbsphere_monitor', ssl_mode: 'disable'
    }, process.env.DBSPHERE_TEST_PG_PASSWORD || 'dbsphere-monitor-test');
    const chain = sample.diagnostics.lockChain.data;
    if (sample.diagnostics.locks.data.blockedSessions > 0 &&
        chain.some(edge => edge.waitingPid === waitingPid && edge.blockerPid === blockerPid)) {
      found = true;
      lockedSample = sample;
      break;
    }
  }
  assert.ok(found, `PG${major} failed to expose the advisory lock blocking chain`);
  console.log(`PG${major} lock chain: blocker ${blockerPid} -> waiter ${waitingPid}`);
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const directory = mkdtempSync(join(root, `dbsphere-pg${major}-rca-`));
  const store = new Store(directory);
  try {
    const asset = store.addAsset({ name: `PG${major} lock RCA`, host: connection.host,
      port: connection.port, database: 'postgres', username: 'dbsphere_monitor',
      sslMode: 'disable', runtime: 'linux' }, 'test-placeholder');
    store.updateAlertRule('pg_lock_wait', { durationSeconds: 0 });
    store.recordResult(asset.id, lockedSample);
    const [summary] = store.listEvents().filter(event => event.assetId === asset.id);
    assert.ok(summary, `PG${major} lock alert did not create an event`);
    const event = store.getEvent(summary.id);
    const referenced = store.getSamplesByIds(asset.id, eventSampleIds(event));
    await blocker.query('SELECT pg_advisory_unlock(1742026)');
    const response = await waiting;
    assert.equal(response.status, 200);
    const traceId = randomBytes(16).toString('hex');
    const requestSpanId = randomBytes(8).toString('hex');
    const dbSpanId = randomBytes(8).toString('hex');
    const nanos = date => String(BigInt(date.getTime()) * 1000000n);
    const input = { resourceSpans: [{ resource: { attributes: [
      { key: 'service.name', value: { stringValue: 'dbsphere-lock-smoke-app' } }] },
    scopeSpans: [{ spans: [
      { traceId, spanId: requestSpanId, kind: 'SPAN_KIND_SERVER',
        startTimeUnixNano: nanos(requestStart), endTimeUnixNano: nanos(requestEnd) },
      { traceId, spanId: dbSpanId, parentSpanId: requestSpanId, kind: 'SPAN_KIND_CLIENT',
        startTimeUnixNano: nanos(dbStart), endTimeUnixNano: nanos(dbEnd), attributes: [
          { key: 'db.system.name', value: { stringValue: 'postgresql' } },
          { key: 'server.address', value: { stringValue: connection.host } },
          { key: 'server.port', value: { intValue: String(connection.port) } },
          { key: 'db.namespace', value: { stringValue: 'postgres|public' } },
          { key: 'db.query.text', value: { stringValue: 'SELECT pg_advisory_lock(1742026)' } }
        ] }
    ] }] }] };
    const parsed = parseOtlpTraces(input, [asset]);
    assert.equal(store.saveTraceSpans(parsed.spans), 2);
    const traceSpans = store.getTraceSpans(asset.id, traceId);
    assert.doesNotMatch(JSON.stringify(traceSpans), /pg_advisory_lock/);
    const analysis = analyzeEvent(event, referenced, { nearbySamples: referenced, traceSpans });
    assert.equal(analysis.eventId, event.id);
    assert.ok(analysis.candidates.some(candidate => candidate.code === 'lock_chain'));
    assert.equal(analysis.traceCoverage.databaseSpans, 1);
    assert.ok(analysis.graph.edges.some(edge => edge.from === `trace_span:${traceId}:${requestSpanId}` &&
      edge.to === `trace_span:${traceId}:${dbSpanId}` && edge.relation === 'parent_of'));
    assert.ok(analysis.graph.edges.some(edge => edge.from === `event:${event.id}` &&
      edge.to === `trace_span:${traceId}:${dbSpanId}` && edge.relation === 'overlaps_db_call'));
    assert.ok(analysis.graph.edges.some(edge => edge.from === `trace_span:${traceId}:${dbSpanId}` &&
      edge.to.startsWith('sample:') && edge.relation === 'overlaps_sample'));
    assert.ok(analysis.graph.edges.some(edge => edge.from.endsWith(`:${waitingPid}`) &&
      edge.to.endsWith(`:${blockerPid}`) && edge.relation === 'blocked_by'));
    console.log(`PG${major} lock RCA: one event ${event.id} links HTTP request, DB Span, waiter, blocker and alert`);
  } finally {
    store.close();
    if (!resolve(directory).startsWith(root + sep)) throw new Error('Unexpected test directory');
    rmSync(directory, { recursive: true, force: true });
  }
} finally {
  await blocker.query('SELECT pg_advisory_unlock(1742026)').catch(() => {});
  await waiting?.catch(() => {});
  if (requestServer) await new Promise(resolve => requestServer.close(resolve));
  await blocker.end();
  await waiter.end();
}
