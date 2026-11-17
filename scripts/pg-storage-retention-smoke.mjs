import assert from 'node:assert/strict';
import pg from 'pg';
import { PgStore } from '../src/pg-store.js';

if (!process.env.DBSPHERE_DATABASE_URL) throw new Error('Set DBSPHERE_DATABASE_URL');
const rootUrl = new URL(process.env.DBSPHERE_DATABASE_URL);
const databaseName = `dbsphere_storage_${process.pid}`;
const admin = new pg.Client({ connectionString: rootUrl.toString() });
await admin.connect();
await admin.query(`CREATE DATABASE ${databaseName}`);
rootUrl.pathname = `/${databaseName}`;
let store;
try {
  store = await PgStore.create(rootUrl.toString());
  const kind = (await store.pool.query("SELECT relkind FROM pg_class WHERE oid='samples'::regclass")).rows[0].relkind;
  assert.equal(kind, 'p', 'Samples must be range partitioned');
  assert.equal((await store.pool.query("SELECT relkind FROM pg_class WHERE oid='events'::regclass")).rows[0].relkind,
    'p', 'Events must be range partitioned');
  const asset = await store.addAsset({ name: 'PG18 retention smoke', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted-placeholder');
  const healthy = await store.recordResult(asset.id, { version_num: 180006, connections: 10, max_connections: 100 });
  const failed = await store.recordResult(asset.id, {}, 'connection refused');
  const eventId = (await store.listEvents())[0].id;
  const old = new Date(Date.now() - 100 * 86400000).toISOString();
  await store.ensureSampleMonth(old);
  await store.pool.query('UPDATE samples SET collected_at=$1 WHERE id IN ($2,$3)',
    [old, healthy.sampleId, failed.sampleId]);
  const trend = await store.aggregateSamples(asset.id, new Date(Date.now() - 101 * 86400000).toISOString(),
    new Date().toISOString(), 24);
  assert.equal(trend.reduce((sum, item) => sum + item.samples, 0), 2);
  assert.equal(trend.reduce((sum, item) => sum + item.errors, 0), 1);
  assert.ok((await store.storageStats()).samplePartitionBytes > 0);
  const first = await store.pruneSamples(90);
  assert.equal(first.removed, 1);
  assert.equal((await store.pool.query('SELECT id FROM samples WHERE id=$1', [failed.sampleId])).rowCount, 1);
  assert.equal((await store.getEvent(eventId)).status, 'open');
  await store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100 });
  assert.equal((await store.getEvent(eventId)).status, 'resolved');
  const second = await store.pruneSamples(90);
  assert.equal(second.removed, 1);
  assert.equal((await store.pool.query('SELECT id FROM samples WHERE id=$1', [failed.sampleId])).rowCount, 0);
  await store.pool.query("UPDATE events SET ended_at=now()-interval '100 days' WHERE id=$1", [eventId]);
  assert.equal((await store.pruneResolvedEvents(90)).removed, 0,
    'Pending notifications must protect a resolved event');
  for (const item of await store.listDueNotifications()) await store.completeNotification(item.id, 'skipped');
  assert.equal((await store.pruneResolvedEvents(90)).removed, 1);
  assert.equal(await store.getEvent(eventId), null);
  console.log('PostgreSQL 18 monthly sample/event partitions, aggregation and protected retention passed');
} finally {
  if (store) await store.close();
  await admin.query(`DROP DATABASE IF EXISTS ${databaseName}`);
  await admin.end();
}
