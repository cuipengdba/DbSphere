import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { collectK8s } from '../src/k8s.js';
import { Store } from '../src/store.js';
import { analyzeEvent, eventSampleIds } from '../src/rca.js';

if (!process.env.DBSPHERE_KUBECONFIG) throw new Error('Set DBSPHERE_KUBECONFIG to a read-only kubeconfig');
const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
mkdirSync(root, { recursive: true });
const directory = mkdtempSync(join(root, 'dbsphere-k8s-rca-'));
const store = new Store(directory);
try {
  const k8s = await collectK8s({ k8s_cluster: 'dbsphere-readonly',
    k8s_namespace: 'dbsphere-smoke', k8s_pod: 'dbsphere-rca-pending' });
  assert.equal(k8s.status, 'ok');
  const condition = k8s.pod?.conditions?.find(item => item.type === 'PodScheduled');
  assert.equal(condition?.status, 'False');
  assert.ok(k8s.events?.some(item => item.reason === 'FailedScheduling'));
  const asset = store.addAsset({ name: 'Scheduling RCA drill', host: '127.0.0.1', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'k8s',
    k8sCluster: 'dbsphere-readonly', namespace: 'dbsphere-smoke', pod: 'dbsphere-rca-pending' },
  'test-only-placeholder');
  store.updateAlertRule('k8s_pod_unscheduled', { durationSeconds: 0 });
  store.recordResult(asset.id, { k8s }, 'connect ECONNREFUSED 127.0.0.1:5432');
  const events = store.listEvents().filter(item => item.assetId === asset.id);
  assert.equal(events.length, 1);
  const event = store.getEvent(events[0].id);
  const samples = store.getSamplesByIds(asset.id, eventSampleIds(event));
  const analysis = analyzeEvent(event, samples, { nearbySamples: samples });
  assert.equal(analysis.eventId, event.id);
  assert.ok(analysis.candidates.some(candidate => candidate.code === 'k8s_scheduling'));
  assert.ok(analysis.candidates.some(candidate => candidate.code === 'k8s_scheduling_event'));
  assert.ok(analysis.graph.nodes.some(node => node.type === 'k8s_event'));
  assert.ok(event.evidence.some(item => item.kind === 'alert_firing' &&
    item.payload.metric === 'pg.unavailable'));
  console.log(`Real kind scheduling failure and PG connection failure share event ${event.id}`);
} finally {
  store.close();
  if (!resolve(directory).startsWith(root + sep)) throw new Error('Unexpected test directory');
  rmSync(directory, { recursive: true, force: true });
}
