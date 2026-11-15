import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve, sep } from 'node:path';
import { collectK8s, discoverK8s } from '../src/k8s.js';
import { createApp } from '../src/server.js';

test('Kubernetes evidence is scoped to the mapped Pod and PVC', async () => {
  const requests = [];
  const api = {
    async readNamespacedPod(query) {
      requests.push(['pod', query]);
      return {
        metadata: { name: 'pg-0', uid: 'pod-uid', labels: { app: 'postgres' }, ownerReferences: [{ name: 'pg' }] },
        spec: { nodeName: 'worker-1', volumes: [{ persistentVolumeClaim: { claimName: 'pg-data' } }] },
        status: { phase: 'Running', containerStatuses: [{ name: 'postgres', ready: true, restartCount: 1, lastState: { terminated: { reason: 'OOMKilled', finishedAt: '2026-11-15T00:00:00Z' } } }] }
      };
    },
    async readNamespacedPersistentVolumeClaim(query) { requests.push(['pvc', query]); return { spec: { volumeName: 'pv-1', resources: { requests: { storage: '10Gi' } } }, status: { phase: 'Bound', capacity: { storage: '20Gi' } } }; },
    async readPersistentVolume(query) { requests.push(['pv', query]); return { metadata: { name: 'pv-1' }, spec: { capacity: { storage: '20Gi' } }, status: { phase: 'Bound' } }; },
    async listNamespacedEvent(query) { requests.push(['events', query]); return { items: [{ reason: 'BackOff', message: 'Pod restarted', lastTimestamp: '2026-11-15T00:01:00Z' }] }; },
    async readNode(query) { requests.push(['node', query]); return { metadata: { name: 'worker-1' }, status: { conditions: [{ type: 'Ready', status: 'True' }, { type: 'MemoryPressure', status: 'False' }], allocatable: { cpu: '4', memory: '8Gi' } } }; },
    async listNamespacedService(query) { requests.push(['services', query]); return { items: [{ metadata: { name: 'postgres' }, spec: { selector: { app: 'postgres' }, ports: [{ port: 5432 }] } }] }; }
  };
  const metrics = {
    async getPodMetrics() { return { items: [{ metadata: { name: 'pg-0' }, timestamp: '2026-11-15T00:02:00Z', containers: [{ name: 'postgres', usage: { cpu: '50m', memory: '100Mi' } }] }] }; },
    async getNodeMetrics() { throw new Error('Node metrics forbidden'); }
  };
  const result = await collectK8s({ k8s_cluster: 'prod', k8s_namespace: 'database', k8s_pod: 'pg-0' }, api, metrics);
  assert.equal(result.status, 'ok');
  assert.equal(result.pod.node, 'worker-1');
  assert.equal(result.containers[0].terminatedReason, 'OOMKilled');
  assert.equal(result.pvcs[0].volume.name, 'pv-1');
  assert.equal(result.pvcs[0].requested, '10Gi');
  assert.equal(result.node.conditions[0].type, 'Ready');
  assert.equal(result.services[0].name, 'postgres');
  assert.equal(result.resourceMetrics.containers[0].memory, '100Mi');
  assert.match(result.nodeError, /Node metrics forbidden/);
  assert.equal(result.events[0].reason, 'BackOff');
  assert.deepEqual(requests[0], ['pod', { namespace: 'database', name: 'pg-0' }]);
  assert.deepEqual(requests[1], ['pvc', { namespace: 'database', name: 'pg-data' }]);
  assert.deepEqual(requests[2], ['pv', { name: 'pv-1' }]);
  assert.equal(requests.find(([kind]) => kind === 'events')[1].fieldSelector, 'involvedObject.uid=pod-uid');
});

test('Kubernetes discovery links ReplicaSet to Deployment and reports missing permissions', async () => {
  const api = {
    async listNamespacedPod() { return { items: [{ metadata: { name: 'pg-abc', uid: 'pod-1', labels: { app: 'pg' }, ownerReferences: [{ kind: 'ReplicaSet', name: 'pg-xyz', uid: 'rs-1', controller: true }] }, spec: { nodeName: 'node-1', containers: [{ name: 'postgres', image: 'postgres:18', ports: [{ containerPort: 5432 }] }], volumes: [{ persistentVolumeClaim: { claimName: 'data' } }] } }] }; },
    async listNamespacedService() { return { items: [{ metadata: { name: 'pg' }, spec: { selector: { app: 'pg' } } }] }; },
    async listNamespacedPersistentVolumeClaim() { throw new Error('PVC list forbidden'); }
  };
  const apps = {
    async listNamespacedReplicaSet() { return { items: [{ metadata: { uid: 'rs-1', ownerReferences: [{ kind: 'Deployment', name: 'pg' }] } }] }; },
    async listNamespacedStatefulSet() { return { items: [] }; },
    async listNamespacedDeployment() { return { items: [{ metadata: { name: 'pg' } }] }; }
  };
  const result = await discoverK8s({ k8s_cluster: 'prod', k8s_namespace: 'database' }, api, apps);
  assert.equal(result.status, 'ok');
  assert.deepEqual(result.pods[0].workload, { kind: 'Deployment', name: 'pg' });
  assert.deepEqual(result.pods[0].services, ['pg']);
  assert.deepEqual(result.pods[0].pvcs, ['data']);
  assert.equal(result.pods[0].postgresLikely, true);
  assert.match(result.errors.pvcs, /forbidden/);
});

test('Kubernetes discovery API requires login and validates the namespace', async () => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  const dataDir = await mkdtemp(join(root, 'dbsphere-discovery-'));
  process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-discovery-test-password';
  const app = await createApp({ dataDir, discoverK8sFn: async asset => ({ status: 'ok', namespace: asset.k8s_namespace, pods: [] }) });
  await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
  const base = `http://127.0.0.1:${app.server.address().port}`;
  try {
    const path = '/api/k8s/discover?cluster=prod&namespace=database';
    assert.equal((await fetch(base + path)).status, 401);
    const login = await fetch(base + '/api/login', { method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ password: process.env.DBSPHERE_ADMIN_PASSWORD }) });
    const cookie = login.headers.get('set-cookie').split(';')[0];
    const discovered = await fetch(base + path, { headers: { cookie } });
    assert.equal(discovered.status, 200);
    assert.equal((await discovered.json()).discovery.namespace, 'database');
    assert.equal((await fetch(base + '/api/k8s/discover?cluster=prod&namespace=bad_name', { headers: { cookie } })).status, 400);
  } finally {
    await app.stop();
    if (!resolve(dataDir).startsWith(root + sep)) throw new Error('Refusing to remove a directory outside the test root');
    await rm(dataDir, { recursive: true, force: true });
  }
});
