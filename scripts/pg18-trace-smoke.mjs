import assert from 'node:assert/strict';
import { PgStore } from '../src/pg-store.js';
import { parseOtlpTraces } from '../src/otel.js';
import { analyzeEvent, eventSampleIds } from '../src/rca.js';

if (!process.env.DBSPHERE_DATABASE_URL) throw new Error('Set DBSPHERE_DATABASE_URL');
const store = await PgStore.create(process.env.DBSPHERE_DATABASE_URL);
let asset;
try {
  asset = await store.addAsset({ name: 'PG18 trace smoke', host: 'trace-smoke.local', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted-test-placeholder');
  const recorded = await store.recordResult(asset.id, {}, 'connection refused');
  const event = (await store.listEvents()).find(item => item.assetId === asset.id);
  assert.ok(event);
  const now = new Date();
  const nanos = date => String(BigInt(date.getTime()) * 1000000n);
  const input = { resourceSpans: [{ resource: { attributes: [
    { key: 'service.name', value: { stringValue: 'checkout' } }] }, scopeSpans: [{ spans: [
      { traceId: 'a'.repeat(32), spanId: 'b'.repeat(16), kind: 'SPAN_KIND_SERVER',
        startTimeUnixNano: nanos(new Date(now.getTime() - 100)), endTimeUnixNano: nanos(now) },
      { traceId: 'a'.repeat(32), spanId: 'c'.repeat(16), parentSpanId: 'b'.repeat(16),
        kind: 'SPAN_KIND_CLIENT', startTimeUnixNano: nanos(new Date(now.getTime() - 50)),
        endTimeUnixNano: nanos(now), attributes: [
          { key: 'db.system.name', value: { stringValue: 'postgresql' } },
          { key: 'server.address', value: { stringValue: 'trace-smoke.local' } },
          { key: 'server.port', value: { intValue: '5432' } },
          { key: 'db.namespace', value: { stringValue: 'postgres|public' } },
          { key: 'db.query.text', value: { stringValue: 'SELECT private_value FROM users' } }
        ] }
    ] }] }] };
  const parsed = parseOtlpTraces(input, [asset]);
  assert.equal(await store.saveTraceSpans(parsed.spans), 2);
  assert.equal(await store.saveTraceSpans(parsed.spans), 0);
  const spans = await store.listTraceSpans(asset.id,
    new Date(now.getTime() - 60000).toISOString(), new Date(now.getTime() + 60000).toISOString());
  assert.equal(spans.length, 2);
  assert.doesNotMatch(JSON.stringify(spans), /private_value|SELECT/);
  const fullEvent = await store.getEvent(event.id);
  const samples = await store.getSamplesByIds(asset.id, eventSampleIds(fullEvent));
  const analysis = analyzeEvent(fullEvent, samples, { traceSpans: spans });
  assert.equal(analysis.eventId, event.id);
  assert.equal(analysis.traceCoverage.databaseSpans, 1);
  assert.ok(analysis.graph.edges.some(edge => edge.relation === 'parent_of'));
  assert.equal((await store.getTraceSpans(asset.id, 'a'.repeat(32))).length, 2);
  console.log('PG18 OTLP trace persisted and linked to the same event ID');
} finally {
  if (asset) await store.deleteAsset(asset.id);
  await store.close();
}
