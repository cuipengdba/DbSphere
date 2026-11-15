import assert from 'node:assert/strict';
import { createApp } from '../src/server.js';
import { encrypt, loadMasterKey } from '../src/security.js';

if (!process.env.DBSPHERE_DATABASE_URL || !process.env.DBSPHERE_TEST_DATA_DIR ||
    !process.env.DBSPHERE_TEST_PG_PORT || !process.env.DBSPHERE_TEST_EXPORTER_PORT) {
  throw new Error('Set DBSPHERE_DATABASE_URL, DBSPHERE_TEST_DATA_DIR, DBSPHERE_TEST_PG_PORT and DBSPHERE_TEST_EXPORTER_PORT');
}
process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-linux-smoke-strong-password';
const dataDir = process.env.DBSPHERE_TEST_DATA_DIR;
let app = await createApp({ dataDir });
await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
const asset = await app.store.addAsset({ name: 'Linux PG smoke', host: '127.0.0.1',
  port: Number(process.env.DBSPHERE_TEST_PG_PORT), database: 'postgres',
  username: 'dbsphere_monitor', sslMode: 'disable', runtime: 'linux',
  linuxHost: '127.0.0.1', linuxExporterPort: Number(process.env.DBSPHERE_TEST_EXPORTER_PORT) },
encrypt(loadMasterKey(dataDir), process.env.DBSPHERE_TEST_PG_PASSWORD || 'dbsphere-monitor-test'));
try {
  assert.equal((await app.collectAsset(asset.id)).status, 'ok');
  await new Promise(resolve => setTimeout(resolve, 1200));
  assert.equal((await app.collectAsset(asset.id)).status, 'ok');
  const samples = await app.store.listSamples(asset.id);
  assert.equal(samples.length, 2);
  const latest = samples[0].metrics;
  assert.equal(latest.linux.status, 'ok');
  assert.ok(latest.linux.memory.totalBytes > 0);
  assert.ok(latest.linux.filesystems.length > 0);
  assert.ok(Number.isFinite(latest.linux.cpu.usedPercent));
  assert.ok(Number.isFinite(latest.linux.disk.readBytesPerSecond));
  assert.ok(Number.isFinite(latest.linux.network.receiveBytesPerSecond));
  assert.equal(latest.pg14_eol, true);
} finally { await app.stop(); }

app = await createApp({ dataDir });
await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
try {
  assert.equal((await app.store.listSamples(asset.id)).length, 2);
  console.log('Linux PG14 + node_exporter + PG18 store: resource rates and restart persistence passed');
} finally { await app.stop(); }
