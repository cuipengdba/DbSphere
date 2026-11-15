import assert from 'node:assert/strict';
import { createApp } from '../src/server.js';
import { encrypt, loadMasterKey } from '../src/security.js';

if (!process.env.DBSPHERE_DATABASE_URL || !process.env.DBSPHERE_TEST_DATA_DIR ||
    !process.env.DBSPHERE_TEST_PG_PORT || !process.env.DBSPHERE_TEST_POOLER_PORT) {
  throw new Error('Set DBSPHERE_DATABASE_URL, DBSPHERE_TEST_DATA_DIR, DBSPHERE_TEST_PG_PORT and DBSPHERE_TEST_POOLER_PORT');
}
process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-pgbouncer-smoke-password';
const dataDir = process.env.DBSPHERE_TEST_DATA_DIR;
let app = await createApp({ dataDir });
const key = loadMasterKey(dataDir);
const poolerUrl = `postgres://dbsphere_monitor:dbsphere-monitor-test@127.0.0.1:${process.env.DBSPHERE_TEST_POOLER_PORT}/pgbouncer`;
const asset = await app.store.addAsset({ name: 'PG14 with PgBouncer', host: '127.0.0.1',
  port: Number(process.env.DBSPHERE_TEST_PG_PORT), database: 'postgres', username: 'dbsphere_monitor',
  sslMode: 'disable', runtime: 'linux' }, encrypt(key, 'dbsphere-monitor-test'), encrypt(key, poolerUrl));
try {
  assert.equal(asset.poolerConfigured, true);
  assert.equal(JSON.stringify(asset).includes('dbsphere-monitor-test'), false);
  const result = await app.collectAsset(asset.id);
  assert.equal(result.status, 'ok');
  const sample = (await app.store.listSamples(asset.id))[0];
  assert.equal(sample.metrics.pgbouncer.status, 'ok');
  assert.ok(sample.metrics.pgbouncer.pools.length > 0);
  assert.equal(sample.metrics.pg14_eol, true);
} finally { await app.stop(); }

app = await createApp({ dataDir });
try {
  assert.equal((await app.store.listSamples(asset.id)).length, 1);
  console.log('PG14 + PgBouncer stats console + PG18 store: read-only collection and restart persistence passed');
} finally { await app.stop(); }
