import test from 'node:test';
import assert from 'node:assert/strict';
import { collectK8s } from '../src/k8s.js';

test('Kubernetes evidence is scoped to the mapped Pod and PVC', async () => {
  const requests = [];
  const api = {
    async readNamespacedPod(query) {
      requests.push(['pod', query]);
      return {
        metadata: { name: 'pg-0', uid: 'pod-uid', ownerReferences: [{ name: 'pg' }] },
        spec: { nodeName: 'worker-1', volumes: [{ persistentVolumeClaim: { claimName: 'pg-data' } }] },
        status: { phase: 'Running', containerStatuses: [{ name: 'postgres', ready: true, restartCount: 1, lastState: { terminated: { reason: 'OOMKilled', finishedAt: '2026-11-15T00:00:00Z' } } }] }
      };
    },
    async readNamespacedPersistentVolumeClaim(query) { requests.push(['pvc', query]); return { status: { phase: 'Bound', capacity: { storage: '20Gi' } } }; },
    async listNamespacedEvent(query) { requests.push(['events', query]); return { items: [{ reason: 'BackOff', message: 'Pod restarted', lastTimestamp: '2026-11-15T00:01:00Z' }] }; }
  };
  const result = await collectK8s({ k8s_cluster: 'prod', k8s_namespace: 'database', k8s_pod: 'pg-0' }, api);
  assert.equal(result.status, 'ok');
  assert.equal(result.pod.node, 'worker-1');
  assert.equal(result.containers[0].terminatedReason, 'OOMKilled');
  assert.deepEqual(result.pvcs, [{ name: 'pg-data', phase: 'Bound', capacity: '20Gi' }]);
  assert.equal(result.events[0].reason, 'BackOff');
  assert.deepEqual(requests[0], ['pod', { namespace: 'database', name: 'pg-0' }]);
  assert.deepEqual(requests[1], ['pvc', { namespace: 'database', name: 'pg-data' }]);
  assert.equal(requests[2][1].fieldSelector, 'involvedObject.uid=pod-uid');
});
