#!/usr/bin/env node

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';

const [baseURL, credentialsPath, kubeconfigPath, cluster = 'agentsql-v350'] = process.argv.slice(2);
if (!baseURL || !credentialsPath || !kubeconfigPath) {
  console.error('usage: node scripts/kind-matrix-acceptance.mjs BASE_URL CREDENTIALS_PATH KUBECONFIG_PATH [KIND_CLUSTER_NAME]');
  process.exit(2);
}
const credentials = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const url = baseURL.replace(/\/$/, '');
let cookie = '';
let csrf = '';
const kubectl = args => execFileSync('kubectl', ['--kubeconfig', kubeconfigPath, '--request-timeout=8s', ...args],
  { encoding: 'utf8', timeout: 12000 }).trim();
function canI(verb, resource, namespace = 'dbsphere-matrix') {
  const namespaced = !['nodes', 'persistentvolumes'].includes(resource);
  const args = ['auth', 'can-i', verb, resource,
    '--as=system:serviceaccount:dbsphere-matrix:dbsphere-reader',
    ...(namespaced ? ['-n', namespace] : [])];
  try { return kubectl(args) === 'yes'; }
  catch (error) {
    if (String(error.stdout).trim() === 'no') return false;
    throw error;
  }
}
async function call(path, method = 'GET', payload) {
  const response = await fetch(url + path, {
    method, headers: {
      ...(payload ? { 'Content-Type': 'application/json' } : {}),
      ...(cookie ? { Cookie: cookie } : {}),
      ...(method !== 'GET' && csrf ? { 'X-CSRF-Token': csrf } : {})
    }, body: payload ? JSON.stringify(payload) : undefined,
    signal: AbortSignal.timeout(20000)
  });
  const body = await response.json();
  if (!response.ok) throw new Error(`${method} ${path}: ${response.status} ${JSON.stringify(body)}`);
  return { body, response };
}

const ready = (await call('/readyz')).body;
assert.equal(ready.storage, 'postgresql18');
for (const [verb, resource] of [['get', 'pods'], ['list', 'events'], ['get', 'nodes'],
  ['get', 'persistentvolumeclaims'], ['get', 'persistentvolumes']]) {
  assert.ok(canI(verb, resource), `read-only ServiceAccount lacks ${verb} ${resource}`);
}
for (const [verb, resource, namespace] of [['get', 'secrets', 'dbsphere-matrix'],
  ['delete', 'pods', 'dbsphere-matrix'], ['list', 'pods', 'agentsql']]) {
  assert.equal(canI(verb, resource, namespace), false,
    `read-only ServiceAccount unexpectedly allows ${verb} ${resource} in ${namespace}`);
}
const pvc = JSON.parse(kubectl(['-n', 'dbsphere-matrix', 'get', 'pvc', '-o', 'json'])).items;
for (const name of ['dbsphere-store-data', 'pg18-data']) {
  assert.equal(pvc.find(item => item.metadata.name === name)?.status.phase, 'Bound', `${name} is not bound`);
}
const login = await call('/api/login', 'POST', { password: credentials.adminPassword });
cookie = login.response.headers.get('set-cookie')?.split(';')[0] || '';
csrf = login.body.csrf;
assert.ok(cookie && csrf, 'admin session cookie and CSRF token must be present');

const existing = (await call('/api/assets')).body.assets;
const report = [];
for (let major = 14; major <= 19; major++) {
  const name = `kind-matrix-pg${major}`;
  let asset = existing.find(item => item.name === name);
  if (!asset) {
    const created = await call('/api/assets', 'POST', {
      name, host: `pg${major}`, port: 5432, database: 'postgres',
      username: 'dbsphere_monitor', password: credentials.monitorPassword,
      sslMode: 'disable', runtime: 'k8s', k8sCluster: `kind-${cluster}`,
      namespace: 'dbsphere-matrix', workload: `pg${major}`
    });
    asset = created.body.asset;
  }
  assert.ok(asset?.id, `${name} asset ID missing`);
  const checked = (await call(`/api/assets/${asset.id}/check`, 'POST')).body.result;
  assert.equal(checked.status, 'ok', `${name} collection failed`);
  const sampled = (await call(`/api/assets/${asset.id}/samples/${checked.sampleId}`)).body.sample;
  const detail = (await call(`/api/assets/${asset.id}`)).body.asset;
  assert.equal(Math.floor(detail.versionNum / 10000), major, `${name} version mismatch`);
  assert.equal(sampled.status, 'ok', `${name} sample status mismatch`);
  assert.equal(Math.floor(sampled.metrics.version_num / 10000), major, `${name} sample version mismatch`);
  assert.equal(sampled.metrics.monitor_role, true, `${name} monitor role missing`);
  assert.equal(sampled.metrics.privileged_role, false, `${name} monitor role is privileged`);
  assert.equal(sampled.metrics.pgCompatibility?.major, major, `${name} capability matrix version mismatch`);
  assert.equal(sampled.metrics.pgCompatibility?.signals.find(signal => signal.key === 'activeWaits')?.status, 'ok');
  assert.equal(sampled.metrics?.k8s?.status, 'ok', `${name} K8S signal missing`);
  assert.match(sampled.metrics.k8s.pod.name, new RegExp(`^pg${major}-`), `${name} pod association wrong`);
  assert.equal(sampled.metrics.k8s.pod.phase, 'Running', `${name} pod is not running`);
  assert.ok(sampled.metrics.k8s.containers?.some(item => item.name === 'postgres' && item.ready),
    `${name} PostgreSQL container is not ready`);
  assert.ok(sampled.metrics.k8s.node?.name?.startsWith(`${cluster}-worker`), `${name} node association missing`);
  assert.ok(sampled.metrics.k8s.services?.some(item => item.name === `pg${major}`), `${name} service association missing`);
  if (major === 18) {
    assert.ok(sampled.metrics.k8s.pvcs?.some(item => item.name === 'pg18-data' && item.phase === 'Bound'
      && item.volume?.phase === 'Bound'), 'PG18 PVC/PV association missing');
  }
  report.push({ major, assetId: asset.id, sampleId: checked.sampleId,
    node: sampled.metrics.k8s.node.name, pod: sampled.metrics.k8s.pod.name });
}
assert.ok(new Set(report.map(item => item.node)).size >= 2, 'PG targets did not span worker nodes');
console.log(JSON.stringify({ storage: ready.storage, persistentClaims: ['dbsphere-store-data', 'pg18-data'],
  matrix: report }, null, 2));
