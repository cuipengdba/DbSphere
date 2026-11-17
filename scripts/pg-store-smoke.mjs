import assert from 'node:assert/strict';
import { createApp } from '../src/server.js';

if (!process.env.DBSPHERE_DATABASE_URL || !process.env.DBSPHERE_TEST_DATA_DIR) {
  throw new Error('Set DBSPHERE_DATABASE_URL and DBSPHERE_TEST_DATA_DIR');
}
process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-store-smoke-strong-password';
const dataDir = process.env.DBSPHERE_TEST_DATA_DIR;
let failures = 2;
const collectFn = async () => {
  if (failures-- > 0) throw new Error('connection refused');
  return { version_num: 180006, role: 'primary', connections: 1, database_bytes: 4096 };
};
const k8sFn = async () => ({ status: 'ok', pod: { name: 'postgres-0', phase: 'Running' },
  containers: [{ name: 'postgres', restartCount: 1, terminatedReason: 'OOMKilled',
    lastTerminatedAt: new Date().toISOString() }], pvcs: [], events: [] });

let app = await createApp({ dataDir, collectFn, k8sFn });
const start = async () => {
  await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
  return `http://127.0.0.1:${app.server.address().port}`;
};
const stop = async () => { await app.stop(); };
const login = async base => {
  const response = await fetch(`${base}/api/login`, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password: process.env.DBSPHERE_ADMIN_PASSWORD })
  });
  assert.equal(response.status, 200);
  return { cookie: response.headers.get('set-cookie').split(';')[0],
    csrf: (await response.json()).csrf };
};

let assetId;
let eventId;
let linuxAssetId;
try {
  const base = await start();
  assert.equal((await fetch(`${base}/readyz`)).status, 200);
  const auth = await login(base);
  const headers = { cookie: auth.cookie, 'x-csrf-token': auth.csrf, 'content-type': 'application/json' };
  const created = await fetch(`${base}/api/assets`, { method: 'POST', headers,
    body: JSON.stringify({ name: 'PG18 store smoke', host: '127.0.0.1', port: 5432,
      database: 'postgres', username: 'monitor', password: 'test-monitor-secret',
      sslMode: 'disable', runtime: 'k8s', k8sCluster: 'smoke', namespace: 'database', pod: 'postgres-0' }) });
  assert.equal(created.status, 201);
  assetId = (await created.json()).asset.id;
  for (const expected of ['error', 'error', 'ok']) {
    const check = await fetch(`${base}/api/assets/${assetId}/check`, { method: 'POST', headers });
    assert.equal(check.status, 200);
    assert.equal((await check.json()).result.status, expected);
  }
  const events = await (await fetch(`${base}/api/events`, { headers: { cookie: auth.cookie } })).json();
  const assetEvents = events.events.filter(event => event.assetId === assetId);
  assert.equal(assetEvents.length, 1);
  assert.equal(assetEvents[0].status, 'resolved');
  eventId = assetEvents[0].id;
  const event = await (await fetch(`${base}/api/events/${eventId}`, { headers: { cookie: auth.cookie } })).json();
  assert.equal(event.event.evidence.filter(e => e.kind === 'root_cause_candidate').length, 1);
  assert.equal(event.event.evidence.filter(e => e.kind === 'collection_error').length, 2);
  assert.equal(event.event.evidence.filter(e => e.kind === 'collection_recovered').length, 1);
  const linuxCreated = await fetch(`${base}/api/assets`, { method: 'POST', headers,
    body: JSON.stringify({ name: 'Linux exporter mapping', host: '127.0.0.1', port: 5432,
      database: 'postgres', username: 'monitor', password: 'test-monitor-secret',
      sslMode: 'disable', runtime: 'linux', linuxHost: 'db-node-01', linuxExporterPort: 9100,
      poolerUrl: 'postgres://monitor:pooler-secret@pooler:6432/pgbouncer' }) });
  assert.equal(linuxCreated.status, 201);
  linuxAssetId = (await linuxCreated.json()).asset.id;
} finally { await stop(); }

app = await createApp({ dataDir, collectFn, k8sFn });
try {
  const base = await start();
  assert.equal((await fetch(`${base}/readyz`)).status, 200);
  const auth = await login(base);
  const assets = await (await fetch(`${base}/api/assets`, { headers: { cookie: auth.cookie } })).json();
  assert.ok(assets.assets.length >= 2);
  assert.equal(assets.assets.find(asset => asset.id === assetId).versionNum, 180006);
  assert.equal(assets.assets.find(asset => asset.id === linuxAssetId).linuxExporterPort, 9100);
  assert.equal(assets.assets.find(asset => asset.id === linuxAssetId).poolerConfigured, true);
  assert.ok(!JSON.stringify(assets).includes('test-monitor-secret'));
  assert.ok(!JSON.stringify(assets).includes('pooler-secret'));
  const samples = await (await fetch(`${base}/api/assets/${assetId}/samples`, {
    headers: { cookie: auth.cookie }
  })).json();
  assert.equal(samples.samples.length, 3);
  const event = await (await fetch(`${base}/api/events/${eventId}`, {
    headers: { cookie: auth.cookie }
  })).json();
  assert.equal(event.event.status, 'resolved');
  assert.equal(event.event.evidence.filter(item => item.kind === 'alert_firing').length, 1);
  assert.equal(event.event.evidence.filter(item => item.kind === 'alert_recovered').length, 1);
  console.log('PostgreSQL 18 store: B/S assets, samples, one event, evidence and restart passed');
} finally { await stop(); }
