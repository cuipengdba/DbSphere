import assert from 'node:assert/strict';
import { collectK8s, discoverK8s } from '../src/k8s.js';

const cluster = process.env.DBSPHERE_TEST_K8S_CONTEXT;
const namespace = process.env.DBSPHERE_TEST_K8S_NAMESPACE;
const podName = process.env.DBSPHERE_TEST_K8S_POD;
if (!process.env.DBSPHERE_KUBECONFIG || !cluster || !namespace || !podName) {
  throw new Error('Set DBSPHERE_KUBECONFIG, DBSPHERE_TEST_K8S_CONTEXT, DBSPHERE_TEST_K8S_NAMESPACE and DBSPHERE_TEST_K8S_POD');
}
const asset = { k8s_cluster: cluster, k8s_namespace: namespace, k8s_pod: podName };
const discovery = await discoverK8s(asset);
assert.equal(discovery.status, 'ok');
const mapped = discovery.pods.find(pod => pod.name === podName);
assert.ok(mapped?.postgresLikely);
assert.ok(mapped.services.includes('postgres'));
assert.ok(mapped.pvcs.includes('postgres-data'));
assert.equal(mapped.workload?.kind, 'Deployment');
assert.equal(discovery.errors.pods, undefined);

const state = await collectK8s(asset);
assert.equal(state.status, 'ok');
assert.equal(state.pod.name, podName);
assert.equal(state.node?.name, mapped.node);
assert.ok(state.node?.conditions.some(condition => condition.type === 'Ready'));
assert.ok(state.pvcs.some(pvc => pvc.name === 'postgres-data' && pvc.volume?.phase === 'Bound'));
assert.ok(state.services.some(service => service.name === 'postgres'));
assert.equal(state.eventsError, null);
assert.ok(state.resourceMetrics || state.resourceMetricsError);
console.log('K8S read-only discovery, Pod/Deployment/Service/Node/PVC/PV association and metrics status passed');
