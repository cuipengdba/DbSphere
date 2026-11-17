import assert from 'node:assert/strict';
import pg from 'pg';
import { PgStore } from '../src/pg-store.js';
import { createNotifier, notificationConfig } from '../src/notifications.js';

if (!process.env.DBSPHERE_DATABASE_URL) throw new Error('Set DBSPHERE_DATABASE_URL');
const rootUrl = new URL(process.env.DBSPHERE_DATABASE_URL);
const databaseName = `dbsphere_notify_${process.pid}`;
const admin = new pg.Client({ connectionString: rootUrl.toString() });
await admin.connect();
await admin.query(`CREATE DATABASE ${databaseName}`);
rootUrl.pathname = `/${databaseName}`;
let store;
try {
  store = await PgStore.create(rootUrl.toString());
  const asset = await store.addAsset({ name: 'PG18 notification smoke', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted-placeholder');
  await store.recordResult(asset.id, {}, 'connection refused');
  const eventId = (await store.listEvents())[0].id;
  assert.equal((await store.listDueNotifications()).length, 2);
  const deliveries = [];
  const notifier = createNotifier(store, notificationConfig({
    DBSPHERE_WEBHOOK_URL: 'https://example.com/hook',
    DBSPHERE_SMTP_HOST: 'mail.example.com', DBSPHERE_SMTP_FROM: 'dbsphere@example.com',
    DBSPHERE_SMTP_TO: 'oncall@example.com'
  }), {
    fetchFn: async (_url, options) => { deliveries.push(JSON.parse(options.body)); return { ok: true }; },
    mailer: { createTransport: () => ({ sendMail: async message => { deliveries.push(message); } }) }
  });
  assert.deepEqual(await notifier.run(), { delivered: 2, failed: 0 });
  assert.equal(deliveries.length, 2);
  assert.equal((await store.listNotificationHistory()).filter(item => item.status === 'delivered').length, 2);
  await store.pool.query("UPDATE events SET started_at=now()-interval '20 minutes' WHERE id=$1", [eventId]);
  assert.equal(await store.queueEscalations(new Date().toISOString(), 15), 1);
  assert.equal(await store.queueEscalations(new Date().toISOString(), 15), 0);
  assert.equal((await store.getEvent(eventId)).evidence.filter(item => item.kind === 'alert_escalation').length, 1);
  assert.deepEqual(await notifier.run(), { delivered: 2, failed: 0 });
  assert.equal(deliveries.length, 4);
  assert.equal((await store.listEvents()).length, 1);
  console.log('PostgreSQL 18 notification outbox, independent channels and one-event escalation passed');
} finally {
  if (store) await store.close();
  await admin.query(`DROP DATABASE IF EXISTS ${databaseName}`);
  await admin.end();
}
