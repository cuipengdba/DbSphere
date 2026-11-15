import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { mkdir, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import pg from 'pg';
import { Store } from '../src/store.js';
import { PgStore } from '../src/pg-store.js';
import { decrypt, encrypt, hashPassword } from '../src/security.js';

if (!process.env.DBSPHERE_DATABASE_URL || !process.env.DBSPHERE_TEST_DATA_DIR) {
  throw new Error('Set DBSPHERE_DATABASE_URL and DBSPHERE_TEST_DATA_DIR');
}
const oldKey = randomBytes(32);
const newKey = randomBytes(32);
const sourceDir = join(resolve(process.env.DBSPHERE_TEST_DATA_DIR), `sqlite-source-${process.pid}`);
await mkdir(sourceDir, { recursive: true, mode: 0o700 });
await writeFile(join(sourceDir, 'master.key'), oldKey, { mode: 0o600, flag: 'wx' });

const source = new Store(sourceDir);
source.setSetting('admin_password_hash', hashPassword('old-admin-password-strong'));
const asset = source.addAsset({ name: 'SQLite source', host: '127.0.0.1', port: 5432,
  database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'k8s',
  k8sCluster: 'source', namespace: 'db', pod: 'postgres-0' }, encrypt(oldKey, 'old-monitor-secret'),
encrypt(oldKey, 'postgres://stats:old-pooler-secret@pooler:6432/pgbouncer'));
source.recordResult(asset.id, { k8s: { status: 'ok', containers: [{ name: 'postgres',
  terminatedReason: 'OOMKilled', lastTerminatedAt: new Date().toISOString() }] } }, 'connection refused');
source.recordResult(asset.id, { version_num: 180006, role: 'primary' });
const sourceEvent = source.listEvents()[0];
source.close();

const databaseName = `dbsphere_migration_${process.pid}`;
const rootUrl = new URL(process.env.DBSPHERE_DATABASE_URL);
const admin = new pg.Client({ connectionString: rootUrl.toString() });
await admin.connect();
try { await admin.query(`CREATE DATABASE ${databaseName}`); }
finally { await admin.end(); }
rootUrl.pathname = `/${databaseName}`;
const targetUrl = rootUrl.toString();
const env = { ...process.env, DBSPHERE_SQLITE_DATA_DIR: sourceDir,
  DBSPHERE_DATABASE_URL: targetUrl, DBSPHERE_MASTER_KEY: newKey.toString('base64') };
const run = () => spawnSync(process.execPath, ['scripts/migrate-sqlite-to-pg18.mjs'], {
  cwd: new URL('..', import.meta.url), env, encoding: 'utf8'
});
const migrated = run();
assert.equal(migrated.status, 0, migrated.stderr);
const duplicate = run();
assert.notEqual(duplicate.status, 0, 'Migration must reject a non-empty destination');

const target = await PgStore.create(targetUrl);
try {
  const imported = await target.getInternalAsset(asset.id);
  assert.equal(decrypt(newKey, imported.password_enc), 'old-monitor-secret');
  assert.equal(decrypt(newKey, imported.pooler_url_enc), 'postgres://stats:old-pooler-secret@pooler:6432/pgbouncer');
  assert.equal((await target.listSamples(asset.id)).length, 2);
  const event = await target.getEvent(sourceEvent.id);
  assert.equal(event.status, 'resolved');
  assert.equal(event.evidence.length, 4);
  assert.equal(event.evidence.filter(e => e.kind === 'root_cause_candidate').length, 1);
  console.log('SQLite -> PG18 migration: IDs, credential re-encryption, samples and evidence passed');
} finally { await target.close(); }
