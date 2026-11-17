import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { createApp } from '../src/server.js';

test('rule edits require login and CSRF, reject unsafe changes, and survive restart', async t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const dataDir = mkdtempSync(join(root, 'dbsphere-alert-rules-'));
  const oldPassword = process.env.DBSPHERE_ADMIN_PASSWORD;
  process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-alert-rules-test-password';
  t.after(() => {
    if (oldPassword === undefined) delete process.env.DBSPHERE_ADMIN_PASSWORD;
    else process.env.DBSPHERE_ADMIN_PASSWORD = oldPassword;
    if (!resolve(dataDir).startsWith(root + sep)) throw new Error('Refusing to remove data outside the test root');
    rmSync(dataDir, { recursive: true, force: true });
  });
  const start = async () => {
    const app = await createApp({ dataDir });
    await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
    return { app, base: `http://127.0.0.1:${app.server.address().port}` };
  };
  let { app, base } = await start();
  try {
    assert.equal((await fetch(`${base}/api/alert-rules`)).status, 401);
    const login = await fetch(`${base}/api/login`, { method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ password: process.env.DBSPHERE_ADMIN_PASSWORD }) });
    const cookie = login.headers.get('set-cookie').split(';')[0];
    const { csrf } = await login.json();
    const path = `${base}/api/alert-rules/pg_connections_high`;
    const request = (payload, token = csrf) => fetch(path, { method: 'PATCH', headers: {
      cookie, 'content-type': 'application/json', 'x-csrf-token': token
    }, body: JSON.stringify(payload) });
    assert.equal((await request({ threshold: 75 }, 'bad')).status, 403);
    assert.equal((await request({ metric: 'pg.execute_sql' })).status, 400);
    assert.equal((await request({ threshold: Number.POSITIVE_INFINITY })).status, 400);
    assert.equal((await request({})).status, 400);
    const changed = await request({ threshold: 75, durationSeconds: 30, severity: 'critical' });
    assert.equal(changed.status, 200);
    assert.equal((await changed.json()).rule.version, 2);
    const rules = await (await fetch(`${base}/api/alert-rules`, { headers: { cookie } })).json();
    assert.equal(rules.rules.find(rule => rule.id === 'pg_connections_high').threshold, 75);
    const asset = app.store.addAsset({ name: 'ack smoke', host: 'localhost', port: 5432,
      database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted-placeholder');
    app.store.recordResult(asset.id, {}, 'connection refused');
    const eventId = app.store.listEvents()[0].id;
    const ackPath = `${base}/api/events/${eventId}/ack`;
    assert.equal((await fetch(ackPath, { method: 'POST', headers: { cookie } })).status, 403);
    assert.equal((await fetch(ackPath, { method: 'POST', headers: { cookie, 'x-csrf-token': csrf } })).status, 200);
    assert.equal((await fetch(ackPath, { method: 'POST', headers: { cookie, 'x-csrf-token': csrf } })).status, 200);
    const event = await (await fetch(`${base}/api/events/${eventId}`, { headers: { cookie } })).json();
    assert.ok(event.event.acknowledgedAt);
    assert.equal(event.event.evidence.filter(item => item.kind === 'event_acknowledged').length, 1);
  } finally { await app.stop(); }

  ({ app, base } = await start());
  try {
    const rules = await app.store.getAlertRules();
    const persisted = rules.find(rule => rule.id === 'pg_connections_high');
    assert.equal(persisted.threshold, 75);
    assert.equal(persisted.durationSeconds, 30);
    assert.equal(persisted.version, 2);
  } finally { await app.stop(); }
});
