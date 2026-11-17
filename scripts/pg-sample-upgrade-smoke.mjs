import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import pg from 'pg';
import { PgStore } from '../src/pg-store.js';

if (!process.env.DBSPHERE_DATABASE_URL) throw new Error('Set DBSPHERE_DATABASE_URL');
const rootUrl = new URL(process.env.DBSPHERE_DATABASE_URL);
const databaseName = `dbsphere_upgrade_${process.pid}`;
const admin = new pg.Client({ connectionString: rootUrl.toString() });
await admin.connect();
await admin.query(`CREATE DATABASE ${databaseName}`);
await admin.query(`ALTER DATABASE ${databaseName} SET timezone TO 'America/Los_Angeles'`);
rootUrl.pathname = `/${databaseName}`;
const legacy = new pg.Client({ connectionString: rootUrl.toString() });
let store;
try {
  await legacy.connect();
  await legacy.query(`CREATE TABLE assets (
    id uuid PRIMARY KEY, name text NOT NULL, host text NOT NULL, port integer NOT NULL,
    database_name text NOT NULL, username text NOT NULL, password_enc text NOT NULL,
    ssl_mode text NOT NULL, runtime text NOT NULL, linux_host text, linux_exporter_port integer,
    pooler_url_enc text, k8s_cluster text, k8s_namespace text, k8s_workload text,
    k8s_pod text, k8s_node text, created_at timestamptz NOT NULL,
    last_sample_at timestamptz,last_status text NOT NULL DEFAULT 'pending',
    last_error text,version_num integer,role text
  );
  CREATE TABLE samples (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    collected_at timestamptz NOT NULL,status text NOT NULL,metrics_json jsonb,error text
  );
  CREATE INDEX samples_asset_time ON samples(asset_id,collected_at DESC);
  CREATE TABLE events (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    kind text NOT NULL,title text NOT NULL,status text NOT NULL,
    started_at timestamptz NOT NULL,ended_at timestamptz,event_group text,acknowledged_at timestamptz
  );
  CREATE INDEX events_asset_status ON events(asset_id,status);
  CREATE UNIQUE INDEX events_one_open_group ON events(asset_id,event_group) WHERE status='open';
  CREATE TABLE evidence (
    id uuid PRIMARY KEY,event_id uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    kind text NOT NULL,observed_at timestamptz NOT NULL,payload_json jsonb NOT NULL
  );`);
  const assetId = randomUUID();
  const sampleId = randomUUID();
  const eventId = randomUUID();
  const oldTime = '2026-08-01T00:30:00.000Z';
  await legacy.query(`INSERT INTO assets(id,name,host,port,database_name,username,password_enc,
    ssl_mode,runtime,created_at,last_sample_at,last_status)
    VALUES($1,'Legacy target','localhost',5432,'postgres','monitor','encrypted-placeholder',
      'disable','linux',$2,$2,'ok')`, [assetId, oldTime]);
  await legacy.query(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
    VALUES($1,$2,$3,'ok',$4)`, [sampleId, assetId, oldTime,
    JSON.stringify({ version_num: 180006, connections: 7 })]);
  await legacy.query(`INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
    VALUES($1,$2,'collection_failure','Legacy outage','open',$3,'availability')`,
  [eventId, assetId, oldTime]);
  await legacy.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
    VALUES($1,$2,'collection_error',$3,$4)`, [randomUUID(), eventId, oldTime,
    JSON.stringify({ sampleId, error: 'connection refused' })]);
  await legacy.end();
  store = await PgStore.create(rootUrl.toString());
  assert.equal((await store.pool.query("SELECT relkind FROM pg_class WHERE oid='samples'::regclass")).rows[0].relkind, 'p');
  assert.equal((await store.pool.query("SELECT relkind FROM pg_class WHERE oid='events'::regclass")).rows[0].relkind, 'p');
  assert.equal((await store.pool.query("SELECT count(*)::int AS count FROM samples")).rows[0].count, 1);
  assert.equal((await store.listSamples(assetId))[0].id, sampleId);
  assert.ok((await store.pool.query("SELECT to_regclass('samples_202608') AS name")).rows[0].name);
  assert.ok((await store.pool.query("SELECT to_regclass('events_202608') AS name")).rows[0].name);
  assert.equal((await store.pool.query("SELECT to_regclass('samples_legacy') AS name")).rows[0].name, null);
  assert.equal((await store.getEvent(eventId)).evidence.filter(item => item.kind === 'collection_error').length, 1);
  assert.equal((await store.pool.query('SELECT event_id FROM open_event_groups WHERE asset_id=$1', [assetId])).rows[0].event_id, eventId);
  await store.recordResult(assetId, {}, 'connection refused');
  assert.equal((await store.listEvents()).filter(event => event.assetId === assetId).length, 1);
  await store.recordResult(assetId, { version_num: 180006, connections: 1, max_connections: 100 });
  assert.equal((await store.getEvent(eventId)).status, 'resolved');
  assert.equal((await store.pool.query('SELECT 1 FROM open_event_groups WHERE asset_id=$1', [assetId])).rowCount, 0);
  console.log('V0.2 ordinary PostgreSQL 18 samples and events upgrade preserved history and event IDs');
} finally {
  if (store) await store.close();
  try { await legacy.end(); } catch { /* already closed */ }
  await admin.query(`DROP DATABASE IF EXISTS ${databaseName}`);
  await admin.end();
}
