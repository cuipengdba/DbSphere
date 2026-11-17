import test from 'node:test';
import assert from 'node:assert/strict';
import { createHmac } from 'node:crypto';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { createServer } from 'node:net';
import { Store } from '../src/store.js';
import { createNotifier, notificationConfig } from '../src/notifications.js';
import { normalizeSilence } from '../src/silences.js';

function fixture(t) {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const dataDir = mkdtempSync(join(root, 'dbsphere-notifications-'));
  const store = new Store(dataDir);
  t.after(() => {
    store.close();
    if (!resolve(dataDir).startsWith(root + sep)) throw new Error('Refusing to remove data outside test root');
    rmSync(dataDir, { recursive: true, force: true });
  });
  const asset = store.addAsset({ name: 'Notification target', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted-placeholder');
  return { store, asset };
}

test('notification config requires a safe webhook and complete SMTP settings', () => {
  assert.throws(() => notificationConfig({ DBSPHERE_WEBHOOK_URL: 'http://example.com/hook' }), /HTTPS/);
  assert.throws(() => notificationConfig({ DBSPHERE_SMTP_HOST: 'mail.example.com' }), /together/);
  assert.equal(notificationConfig({ DBSPHERE_WEBHOOK_URL: 'https://example.com/hook' }).webhookUrl,
    'https://example.com/hook');
  assert.equal(notificationConfig({ DBSPHERE_WEBHOOK_MIN_SEVERITY: 'critical' }).webhookMinimumSeverity, 'critical');
  assert.throws(() => notificationConfig({ DBSPHERE_SMTP_ALLOW_PLAIN: 'true',
    DBSPHERE_SMTP_HOST: 'smtp.example.com', DBSPHERE_SMTP_FROM: 'a@example.com',
    DBSPHERE_SMTP_TO: 'b@example.com' }), /loopback/);
});

test('Nodemailer sends an incident email through a real local SMTP conversation', async t => {
  const { store, asset } = fixture(t);
  let mail = '';
  const smtp = createServer(socket => {
    socket.write('220 localhost DbSphere test SMTP\r\n');
    let buffer = '';
    let data = false;
    socket.on('data', chunk => {
      buffer += chunk.toString('utf8');
      for (;;) {
        if (data) {
          const end = buffer.indexOf('\r\n.\r\n');
          if (end < 0) break;
          mail += buffer.slice(0, end);
          buffer = buffer.slice(end + 5);
          data = false;
          socket.write('250 2.0.0 queued\r\n');
          continue;
        }
        const end = buffer.indexOf('\r\n');
        if (end < 0) break;
        const line = buffer.slice(0, end);
        buffer = buffer.slice(end + 2);
        if (/^EHLO|^HELO/i.test(line)) socket.write('250 localhost\r\n');
        else if (/^DATA$/i.test(line)) { data = true; socket.write('354 End data with <CR><LF>.<CR><LF>\r\n'); }
        else if (/^QUIT$/i.test(line)) { socket.end('221 bye\r\n'); break; }
        else socket.write('250 ok\r\n');
      }
    });
  });
  await new Promise(resolve => smtp.listen(0, '127.0.0.1', resolve));
  t.after(() => new Promise(resolve => smtp.close(resolve)));
  store.recordResult(asset.id, {}, 'connection refused');
  const eventId = store.listEvents()[0].id;
  const notifier = createNotifier(store, notificationConfig({ DBSPHERE_SMTP_ALLOW_PLAIN: 'true',
    DBSPHERE_SMTP_HOST: '127.0.0.1', DBSPHERE_SMTP_PORT: String(smtp.address().port),
    DBSPHERE_SMTP_FROM: 'dbsphere@example.com', DBSPHERE_SMTP_TO: 'oncall@example.com' }));
  assert.deepEqual(await notifier.run(), { delivered: 1, failed: 0 });
  assert.match(mail, new RegExp(eventId));
  assert.match(mail, /oncall@example.com/);
  assert.equal(store.listNotificationHistory().find(item => item.channel === 'email').status, 'delivered');
});

test('webhook and email deliver independently with event ID and retry only failed channel', async t => {
  const { store, asset } = fixture(t);
  store.recordResult(asset.id, {}, 'connection refused');
  const eventId = store.listEvents()[0].id;
  const queued = store.listDueNotifications();
  assert.equal(queued.length, 2);
  const sent = [];
  let firstWebhook = true;
  const notifier = createNotifier(store, notificationConfig({
    DBSPHERE_WEBHOOK_URL: 'https://example.com/hook', DBSPHERE_WEBHOOK_SECRET: 'test-secret',
    DBSPHERE_SMTP_HOST: 'mail.example.com', DBSPHERE_SMTP_FROM: 'dbsphere@example.com',
    DBSPHERE_SMTP_TO: 'oncall@example.com'
  }), {
    fetchFn: async (_url, options) => {
      if (firstWebhook) { firstWebhook = false; return { ok: false, status: 503 }; }
      const body = JSON.parse(options.body);
      assert.equal(body.eventId, eventId);
      assert.equal(options.headers['x-dbsphere-signature'], `sha256=${createHmac('sha256', 'test-secret').update(options.body).digest('hex')}`);
      sent.push(['webhook', body]);
      return { ok: true, status: 200 };
    },
    mailer: { createTransport: () => ({ sendMail: async message => { sent.push(['email', message]); } }) }
  });
  assert.deepEqual(await notifier.run(), { delivered: 1, failed: 1 });
  assert.equal(sent.filter(([channel]) => channel === 'email').length, 1);
  assert.equal(store.listDueNotifications().length, 0);
  const retryAt = new Date(Date.now() + 60000).toISOString();
  const retry = store.listDueNotifications(retryAt);
  assert.equal(retry.length, 1);
  store.db.prepare('UPDATE notification_outbox SET next_attempt_at=? WHERE id=?')
    .run(new Date(Date.now() - 1000).toISOString(), retry[0].id);
  assert.deepEqual(await notifier.run(), { delivered: 1, failed: 0 });
  assert.equal(sent.filter(([channel]) => channel === 'webhook').length, 1);
  assert.equal(sent.filter(([channel]) => channel === 'email').length, 1);
  store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100 });
  assert.equal(store.listDueNotifications().length, 2);
});

test('maintenance silence keeps evidence but creates no outbound delivery', t => {
  const { store, asset } = fixture(t);
  store.createSilence(normalizeSilence({ kind: 'maintenance', assetId: asset.id,
    reason: 'planned work', endsAt: new Date(Date.now() + 3600000).toISOString() }));
  store.recordResult(asset.id, {}, 'connection refused');
  assert.equal(store.listDueNotifications().length, 0);
  assert.ok(store.listEvents().length);
});

test('an unacknowledged critical incident escalates once under the same event ID', t => {
  const { store, asset } = fixture(t);
  store.recordResult(asset.id, {}, 'connection refused');
  const eventId = store.listEvents()[0].id;
  store.db.prepare('UPDATE events SET started_at=? WHERE id=?')
    .run(new Date(Date.now() - 20 * 60000).toISOString(), eventId);
  assert.equal(store.queueEscalations(new Date().toISOString(), 15), 1);
  assert.equal(store.queueEscalations(new Date().toISOString(), 15), 0);
  assert.equal(store.getEvent(eventId).evidence.filter(item => item.kind === 'alert_escalation').length, 1);
  assert.equal(store.listDueNotifications().filter(item => item.kind === 'alert_escalation').length, 2);
  assert.equal(store.listEvents().length, 1);
});

test('acknowledgement and active silence prevent escalation', t => {
  const { store, asset } = fixture(t);
  store.recordResult(asset.id, {}, 'connection refused');
  const eventId = store.listEvents()[0].id;
  store.db.prepare('UPDATE events SET started_at=? WHERE id=?')
    .run(new Date(Date.now() - 20 * 60000).toISOString(), eventId);
  const silence = store.createSilence(normalizeSilence({ assetId: asset.id, reason: 'investigating',
    endsAt: new Date(Date.now() + 3600000).toISOString() }));
  assert.equal(store.queueEscalations(), 0);
  store.revokeSilence(silence.id);
  store.acknowledgeEvent(eventId);
  assert.equal(store.queueEscalations(), 0);
});

test('a failed delivery can be explicitly retried without creating another event', t => {
  const { store, asset } = fixture(t);
  store.recordResult(asset.id, {}, 'connection refused');
  const item = store.listDueNotifications().find(notification => notification.channel === 'webhook');
  for (let attempt = 0; attempt < 5; attempt++) store.completeNotification(item.id, 'Webhook HTTP 503');
  assert.equal(store.listNotificationHistory().find(notification => notification.id === item.id).status, 'failed');
  assert.equal(store.retryNotification(item.id), true);
  assert.equal(store.retryNotification(item.id), false);
  assert.ok(store.listDueNotifications().some(notification => notification.id === item.id));
  assert.equal(store.listEvents().length, 1);
});
