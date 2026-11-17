import assert from 'node:assert/strict';
import { collectK8s } from '../src/k8s.js';

if (!process.env.DBSPHERE_KUBECONFIG || !process.env.DBSPHERE_TEST_K8S_CONTEXT) {
  throw new Error('Set DBSPHERE_KUBECONFIG and DBSPHERE_TEST_K8S_CONTEXT');
}

const result = await collectK8s({
  k8s_cluster: process.env.DBSPHERE_TEST_K8S_CONTEXT,
  k8s_namespace: 'dbsphere-smoke',
  k8s_pod: 'dbsphere-probe-failure'
});
assert.equal(result.status, 'ok');
assert.equal(result.pod.phase, 'Running');
assert.equal(result.containers[0].ready, false);
assert.ok(result.pod.conditions.some(condition => condition.type === 'Ready' && condition.status === 'False'));
assert.ok(result.events.some(event => event.reason === 'Unhealthy' && /Readiness probe failed/.test(event.message)));
console.log('Real Kubernetes readiness failure and Unhealthy event were collected');
