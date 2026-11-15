import assert from 'node:assert/strict';
import { collectK8s } from '../src/k8s.js';
import { collectPostgres } from '../src/postgres.js';
import { createApp } from '../src/server.js';

const cluster = process.env.DBSPHERE_TEST_K8S_CONTEXT;
const pod = process.env.DBSPHERE_TEST_K8S_POD;
if (!cluster || !pod || !process.env.DBSPHERE_KUBECONFIG) {
  throw new Error('Set DBSPHERE_KUBECONFIG, DBSPHERE_TEST_K8S_CONTEXT and DBSPHERE_TEST_K8S_POD');
}
const asset = {
  k8s_cluster: cluster, k8s_namespace: 'dbsphere-smoke', k8s_pod: pod,
  host: '127.0.0.1', port: Number(process.env.DBSPHERE_TEST_PG_PORT || 15433),
  database_name: 'postgres', username: 'dbsphere_monitor', ssl_mode: 'disable'
};
const infrastructure = await collectK8s(asset);
assert.equal(infrastructure.status, 'ok');
assert.equal(infrastructure.pod.name, pod);
assert.equal(infrastructure.pod.phase, 'Running');
assert.ok(infrastructure.containers.some(c => c.name === 'postgres' && c.ready));
if (process.env.DBSPHERE_TEST_EXPECT_RESTART === 'true') {
  assert.ok(infrastructure.containers.some(c => c.name === 'postgres' && c.restartCount > 0 && c.lastTerminatedAt));
}
assert.ok(infrastructure.pvcs.some(pvc => pvc.name === 'postgres-data' && pvc.phase === 'Bound'));
assert.equal(infrastructure.eventsError, null);

const database = await collectPostgres(asset, 'dbsphere-monitor-test');
assert.equal(Math.floor(database.version_num / 10000), 18);
assert.equal(database.monitor_role, true);
assert.equal(database.role, 'primary');
assert.ok(database.connections > 0);
assert.equal(database.diagnostics.lockChain.status, 'ok');
console.log(`K8S PostgreSQL smoke passed: Pod ${pod}, PVC Bound, PG18 read-only collection`);

if (process.env.DBSPHERE_TEST_DATA_DIR) {
  process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-k8s-smoke-strong-password';
  const dataDir = process.env.DBSPHERE_TEST_DATA_DIR;
  let app = await createApp({ dataDir });
  const listen = () => new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
  const close = async () => {
    const closed = new Promise(resolve => app.server.once('close', resolve));
    app.stop();
    await closed;
  };
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
  try {
    await listen();
    const base = `http://127.0.0.1:${app.server.address().port}`;
    const auth = await login(base);
    const created = await fetch(`${base}/api/assets`, {
      method: 'POST', headers: { cookie: auth.cookie, 'x-csrf-token': auth.csrf,
        'content-type': 'application/json' },
      body: JSON.stringify({ name: 'K8S PG18 smoke', host: asset.host, port: asset.port,
        database: 'postgres', username: 'dbsphere_monitor', password: 'dbsphere-monitor-test',
        sslMode: 'disable', runtime: 'k8s', k8sCluster: cluster,
        namespace: 'dbsphere-smoke', workload: 'postgres', pod })
    });
    assert.equal(created.status, 201);
    assetId = (await created.json()).asset.id;
    const checked = await fetch(`${base}/api/assets/${assetId}/check`, {
      method: 'POST', headers: { cookie: auth.cookie, 'x-csrf-token': auth.csrf }
    });
    assert.equal(checked.status, 200);
    assert.equal((await checked.json()).result.status, 'ok');
    const samples = await (await fetch(`${base}/api/assets/${assetId}/samples`, {
      headers: { cookie: auth.cookie }
    })).json();
    assert.equal(samples.samples[0].metrics.k8s.pod.name, pod);
    assert.equal(Math.floor(samples.samples[0].metrics.version_num / 10000), 18);
  } finally { await close(); }

  app = await createApp({ dataDir });
  try {
    await listen();
    const base = `http://127.0.0.1:${app.server.address().port}`;
    const auth = await login(base);
    const persisted = await (await fetch(`${base}/api/assets/${assetId}/samples`, {
      headers: { cookie: auth.cookie }
    })).json();
    assert.equal(persisted.samples.length, 1);
    assert.equal(persisted.samples[0].status, 'ok');
    console.log(`DbSphere B/S asset, K8S evidence and PG sample persisted after restart: ${assetId}`);
  } finally { await close(); }
}
