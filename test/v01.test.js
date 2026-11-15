import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { createApp } from '../src/server.js';

const base = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
mkdirSync(base, { recursive: true });

test('B/S API keeps secrets private and groups one failure into one event', async (t) => {
  const dir = mkdtempSync(join(base, 'dbsphere-test-'));
  const previous = process.env.DBSPHERE_ADMIN_PASSWORD;
  process.env.DBSPHERE_ADMIN_PASSWORD = 'test-password-strong-123';
  let failures = 2;
  const passwords = [];
  const app = await createApp({ dataDir: dir, k8sFn: async () => ({
    status: 'ok', pod: { name: 'pg-0', phase: 'Running', node: 'worker-1' },
    containers: [{ name: 'postgres', terminatedReason: 'OOMKilled', lastTerminatedAt: new Date().toISOString(), restartCount: 1 }],
    pvcs: [], events: []
  }), collectFn: async (_asset, password) => {
    passwords.push(password);
    if (failures-- > 0) throw new Error('connection refused');
    return { version_num: 140025, pg14_eol: true, role: 'primary', connections: 3, database_bytes: 4096 };
  }});
  t.after(() => {
    app.stop();
    if (previous === undefined) delete process.env.DBSPHERE_ADMIN_PASSWORD;
    else process.env.DBSPHERE_ADMIN_PASSWORD = previous;
    if (dir.startsWith(base + sep)) rmSync(dir, { recursive: true, force: true });
  });
  await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
  const baseUrl = `http://127.0.0.1:${app.server.address().port}`;
  const request = (path, options) => fetch(baseUrl + path, options);

  assert.equal((await request('/api/assets')).status, 401);
  const login = await request('/api/login', { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ password: 'test-password-strong-123' }) });
  assert.equal(login.status, 200);
  const { csrf } = await login.json();
  const cookie = login.headers.get('set-cookie').split(';')[0];
  const headers = { cookie, 'x-csrf-token': csrf, 'content-type': 'application/json' };
  assert.equal((await request('/api/assets', { method: 'POST', headers: { cookie, 'content-type': 'application/json' }, body: '{}' })).status, 403);

  const created = await request('/api/assets', { method: 'POST', headers, body: JSON.stringify({
    name: 'PG in K8S', host: '127.0.0.1', port: 5432, database: 'postgres', username: 'monitor',
    password: 'secret-should-not-leak', sslMode: 'disable', runtime: 'k8s', k8sCluster: 'prod', namespace: 'database', pod: 'pg-0'
  }) });
  assert.equal(created.status, 201);
  const { asset } = await created.json();
  assert.equal(asset.password, undefined);
  assert.equal(asset.namespace, 'database');

  const updated = await request(`/api/assets/${asset.id}`, { method: 'PATCH', headers, body: JSON.stringify({
    name: 'PG in K8S edited', host: '127.0.0.1', port: 5432, database: 'postgres', username: 'monitor',
    password: '', sslMode: 'disable', runtime: 'k8s', k8sCluster: 'prod', namespace: 'database', pod: 'pg-0'
  }) });
  assert.equal(updated.status, 200);
  assert.equal((await updated.json()).asset.name, 'PG in K8S edited');

  await request(`/api/assets/${asset.id}/check`, { method: 'POST', headers });
  await request(`/api/assets/${asset.id}/check`, { method: 'POST', headers });
  let { events } = await (await request('/api/events', { headers: { cookie } })).json();
  assert.equal(events.length, 1);
  assert.equal(events[0].status, 'open');
  const eventId = events[0].id;
  const event = await (await request(`/api/events/${eventId}`, { headers: { cookie } })).json();
  assert.equal(event.event.evidence.filter(e => e.kind === 'collection_error').length, 2);
  assert.equal(event.event.evidence.filter(e => e.kind === 'k8s_state').length, 2);
  assert.equal(event.event.evidence.filter(e => e.kind === 'root_cause_candidate').length, 1);
  assert.equal(event.event.evidence.find(e => e.kind === 'root_cause_candidate').payload.confidence, 'medium');
  assert.ok(!JSON.stringify(event).includes('secret-should-not-leak'));

  await request(`/api/assets/${asset.id}/check`, { method: 'POST', headers });
  events = (await (await request('/api/events', { headers: { cookie } })).json()).events;
  assert.equal(events.length, 1);
  assert.equal(events[0].id, eventId);
  assert.equal(events[0].status, 'resolved');
  const { samples } = await (await request(`/api/assets/${asset.id}/samples`, { headers: { cookie } })).json();
  assert.equal(samples.length, 3);
  assert.deepEqual(passwords, ['secret-should-not-leak', 'secret-should-not-leak', 'secret-should-not-leak']);
  const { assets } = await (await request('/api/assets', { headers: { cookie } })).json();
  assert.equal(assets[0].pg14Eol, true);
  assert.ok(!JSON.stringify(assets).includes('secret-should-not-leak'));
});
