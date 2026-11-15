import { DatabaseSync } from 'node:sqlite';
import { existsSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { PgStore } from '../src/pg-store.js';
import { decrypt, encrypt } from '../src/security.js';

const sourceDir = process.env.DBSPHERE_SQLITE_DATA_DIR;
const targetUrl = process.env.DBSPHERE_DATABASE_URL;
const targetKeyText = process.env.DBSPHERE_MASTER_KEY;
if (!sourceDir || !targetUrl || !targetKeyText) {
  throw new Error('Set DBSPHERE_SQLITE_DATA_DIR, DBSPHERE_DATABASE_URL and DBSPHERE_MASTER_KEY');
}
const sourcePath = join(resolve(sourceDir), 'dbsphere.sqlite');
if (!existsSync(sourcePath)) throw new Error('SQLite source database not found');

const oldKeyText = process.env.DBSPHERE_OLD_MASTER_KEY;
const oldKey = oldKeyText ? Buffer.from(oldKeyText, 'base64') : readFileSync(join(resolve(sourceDir), 'master.key'));
const targetKey = Buffer.from(targetKeyText, 'base64');
if (oldKey.length !== 32 || targetKey.length !== 32) throw new Error('Source and target master keys must be 32 bytes');

const source = new DatabaseSync(sourcePath, { readOnly: true });
const target = await PgStore.create(targetUrl);
const tables = ['settings', 'assets', 'samples', 'events', 'evidence'];
const sourceRows = Object.fromEntries(tables.map(table => [table, source.prepare(`SELECT * FROM ${table}`).all()]));
const client = await target.pool.connect();
try {
  await client.query('BEGIN');
  await client.query('SELECT pg_advisory_xact_lock(1742026)');
  for (const table of tables.slice(1)) {
    const count = Number((await client.query(`SELECT count(*)::int AS count FROM ${table}`)).rows[0].count);
    if (count > 0) throw new Error(`Target ${table} is not empty; migration is append-unsafe`);
  }
  for (const row of sourceRows.settings) {
    await client.query(`INSERT INTO settings(key,value) VALUES($1,$2)
      ON CONFLICT(key) DO UPDATE SET value=excluded.value`, [row.key, row.value]);
  }
  for (const row of sourceRows.assets) {
    const reencrypted = encrypt(targetKey, decrypt(oldKey, row.password_enc));
    await client.query(`INSERT INTO assets
      (id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,linux_host,linux_exporter_port,
       k8s_cluster,k8s_namespace,k8s_workload,k8s_pod,k8s_node,created_at,last_sample_at,
       last_status,last_error,version_num,role)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`, [
      row.id, row.name, row.host, row.port, row.database_name, row.username, reencrypted,
      row.ssl_mode, row.runtime, row.linux_host, row.linux_exporter_port ?? null, row.k8s_cluster, row.k8s_namespace,
      row.k8s_workload, row.k8s_pod, row.k8s_node, row.created_at, row.last_sample_at,
      row.last_status, row.last_error, row.version_num, row.role
    ]);
  }
  for (const row of sourceRows.samples) {
    await client.query(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json,error)
      VALUES($1,$2,$3,$4,$5,$6)`, [row.id, row.asset_id, row.collected_at,
      row.status, row.metrics_json, row.error]);
  }
  for (const row of sourceRows.events) {
    await client.query(`INSERT INTO events(id,asset_id,kind,title,status,started_at,ended_at)
      VALUES($1,$2,$3,$4,$5,$6,$7)`, [row.id, row.asset_id, row.kind,
      row.title, row.status, row.started_at, row.ended_at]);
  }
  for (const row of sourceRows.evidence) {
    await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
      VALUES($1,$2,$3,$4,$5)`, [row.id, row.event_id, row.kind,
      row.observed_at, row.payload_json]);
  }
  for (const table of tables.slice(1)) {
    const count = Number((await client.query(`SELECT count(*)::int AS count FROM ${table}`)).rows[0].count);
    if (count !== sourceRows[table].length) throw new Error(`Count mismatch in ${table}`);
  }
  await client.query('COMMIT');
  console.log(`Migrated SQLite to PostgreSQL 18: ${tables.map(table => `${table}=${sourceRows[table].length}`).join(', ')}`);
} catch (error) { await client.query('ROLLBACK'); throw error; }
finally { client.release(); source.close(); await target.close(); }
