import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { createApp } from '../src/server.js';

test('notification history and retry require administrator session and CSRF', async t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const dataDir = mkdtempSync(join(root, 'dbsphere-notify-api-'));
  const oldPassword = process.env.DBSPHERE_ADMIN_PASSWORD;
  process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-notify-api-password';
  const app = await createApp({ dataDir });
  t.after(async () => {
    await app.stop();
    if (oldPassword === undefined) delete process.env.DBSPHERE_ADMIN_PASSWORD;
    else process.env.DBSPHERE_ADMIN_PASSWORD = oldPassword;
    if (!resolve(dataDir).startsWith(root + sep)) throw new Error('Refusing to remove data outside test root');
    rmSync(dataDir, { recursive: true, force: true });
  });
  await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
  const base = `http://127.0.0.1:${app.server.address().port}`;
  const asset = app.store.addAsset({ name: 'Notification API target', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted-placeholder');
  app.store.recordResult(asset.id, {}, 'connection refused');
  const notification = app.store.listDueNotifications().find(item => item.channel === 'webhook');
  for (let attempt = 0; attempt < 5; attempt++) app.store.completeNotification(notification.id, 'Webhook HTTP 503');
  assert.equal((await fetch(`${base}/api/notifications`)).status, 401);
  const login = await fetch(`${base}/api/login`, { method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password: process.env.DBSPHERE_ADMIN_PASSWORD }) });
  const cookie = login.headers.get('set-cookie').split(';')[0];
  const { csrf } = await login.json();
  const history = await (await fetch(`${base}/api/notifications`, { headers: { cookie } })).json();
  assert.equal(history.notifications.find(item => item.id === notification.id).status, 'failed');
  assert.equal((await fetch(`${base}/api/notifications/${notification.id}/retry`, {
    method: 'POST', headers: { cookie }
  })).status, 403);
  assert.equal((await fetch(`${base}/api/notifications/${notification.id}/retry`, {
    method: 'POST', headers: { cookie, 'x-csrf-token': csrf }
  })).status, 200);
});
