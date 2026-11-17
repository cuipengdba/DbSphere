import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { parseOtlpTraces } from '../src/otel.js';
import { createApp } from '../src/server.js';

const traceId = 'a'.repeat(32);
const rootId = 'b'.repeat(16);
const dbId = 'c'.repeat(16);
const attr = (key, value) => ({ key, value: { stringValue: value } });
const nanos = date => (BigInt(date.getTime()) * 1000000n).toString();
const payload = (when = new Date(), address = 'pg.example') => ({ resourceSpans: [{
  resource: { attributes: [attr('service.name', 'orders-api')] },
  scopeSpans: [{ spans: [
    { traceId, spanId: rootId, kind: 'SPAN_KIND_SERVER', name: 'GET /users/secret-token',
      startTimeUnixNano: nanos(new Date(when.getTime() - 100)), endTimeUnixNano: nanos(when),
      attributes: [attr('url.full', 'https://example.test/users/secret-token')] },
    { traceId, spanId: dbId, parentSpanId: rootId, kind: 'SPAN_KIND_CLIENT',
      name: 'SELECT password FROM users', startTimeUnixNano: nanos(new Date(when.getTime() - 50)),
      endTimeUnixNano: nanos(when), attributes: [attr('db.system.name', 'postgresql'),
        attr('server.address', address), attr('server.port', '5432'),
        attr('db.namespace', 'postgres|public'),
        attr('db.query.text', 'SELECT password FROM users WHERE secret=12345'),
        attr('db.query.parameter.0', 'secret-value')] }
  ] }] }] });

test('OTLP JSON maps one PostgreSQL client trace without retaining SQL or URL secrets', () => {
  const parsed = parseOtlpTraces(payload(), [{ id: 'asset-1', host: 'pg.example',
    port: 5432, database: 'postgres' }]);
  assert.equal(parsed.spans.length, 2);
  assert.equal(parsed.spans.find(span => span.kind === 'postgresql_client').parentSpanId, rootId);
  assert.equal(parsed.spans[0].assetId, 'asset-1');
  assert.doesNotMatch(JSON.stringify(parsed), /password|secret|SELECT|url\.full/);
  assert.equal(parseOtlpTraces(payload(new Date(), 'other.example'), [{ id: 'asset-1',
    host: 'pg.example', port: 5432, database: 'postgres' }]).spans.length, 0);
});

test('one distributed trace keeps each PostgreSQL client span on its own asset', () => {
  const input = payload();
  input.resourceSpans[0].scopeSpans[0].spans.push({
    ...input.resourceSpans[0].scopeSpans[0].spans[1], spanId: 'd'.repeat(16),
    attributes: [attr('db.system.name', 'postgresql'),
      attr('server.address', 'other.example'), attr('server.port', '5432'),
      attr('db.namespace', 'postgres|public')]
  });
  const parsed = parseOtlpTraces(input, [
    { id: 'asset-1', host: 'pg.example', port: 5432, database: 'postgres' },
    { id: 'asset-2', host: 'other.example', port: 5432, database: 'postgres' }
  ]);
  assert.equal(parsed.rejected, 0);
  assert.deepEqual(parsed.spans.filter(span => span.assetId === 'asset-1').map(span => span.spanId).sort(),
    [rootId, dbId].sort());
  assert.deepEqual(parsed.spans.filter(span => span.assetId === 'asset-2').map(span => span.spanId).sort(),
    [rootId, 'd'.repeat(16)].sort());
});

test('token-protected trace intake joins a PostgreSQL event on the same event ID', async t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const directory = mkdtempSync(join(root, 'dbsphere-otel-'));
  const prior = { password: process.env.DBSPHERE_ADMIN_PASSWORD,
    token: process.env.DBSPHERE_OTLP_TOKEN };
  process.env.DBSPHERE_ADMIN_PASSWORD = 'otel-test-admin-password';
  process.env.DBSPHERE_OTLP_TOKEN = 'otel-test-token-with-at-least-32-characters';
  const app = await createApp({ dataDir: directory });
  t.after(async () => {
    await app.stop();
    for (const [key, value] of Object.entries({ DBSPHERE_ADMIN_PASSWORD: prior.password,
      DBSPHERE_OTLP_TOKEN: prior.token })) {
      if (value === undefined) delete process.env[key]; else process.env[key] = value;
    }
    if (!resolve(directory).startsWith(root + sep)) throw new Error('Unexpected test directory');
    rmSync(directory, { recursive: true, force: true });
  });
  await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
  const url = `http://127.0.0.1:${app.server.address().port}`;
  const asset = app.store.addAsset({ name: 'PG', host: 'pg.example', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted');
  const now = new Date();
  await app.store.recordResult(asset.id, {}, 'connection refused');
  const event = app.store.listEvents().find(item => item.assetId === asset.id);
  assert.ok(event);
  const send = authorization => fetch(`${url}/v1/traces`, { method: 'POST',
    headers: { 'content-type': 'application/json', ...(authorization ? { authorization } : {}) },
    body: JSON.stringify(payload(now)) });
  assert.equal((await send(null)).status, 401);
  assert.equal((await send('Bearer wrong')).status, 401);
  assert.deepEqual(await (await send(`Bearer ${process.env.DBSPHERE_OTLP_TOKEN}`)).json(), {});
  assert.deepEqual(await (await send(`Bearer ${process.env.DBSPHERE_OTLP_TOKEN}`)).json(), {});
  const login = await fetch(`${url}/api/login`, { method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password: process.env.DBSPHERE_ADMIN_PASSWORD }) });
  const cookie = login.headers.get('set-cookie').split(';')[0];
  const detail = await (await fetch(`${url}/api/events/${event.id}`, { headers: { cookie } })).json();
  assert.equal(detail.analysis.eventId, event.id);
  assert.equal(detail.analysis.traceCoverage.databaseSpans, 1);
  assert.ok(detail.analysis.graph.edges.some(edge => edge.from ===
    `trace_span:${traceId}:${rootId}` && edge.to === `trace_span:${traceId}:${dbId}`));
  const source = `/api/assets/${asset.id}/traces/${traceId}`;
  assert.equal((await fetch(url + source)).status, 401);
  const raw = await (await fetch(url + source, { headers: { cookie } })).json();
  assert.equal(raw.spans.length, 2);
  assert.doesNotMatch(JSON.stringify(raw), /password|secret|SELECT/);
});
