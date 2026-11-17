import assert from 'node:assert/strict';
import { PgStore } from '../src/pg-store.js';

if (!process.env.DBSPHERE_DATABASE_URL) throw new Error('Set DBSPHERE_DATABASE_URL');
const store = await PgStore.create(process.env.DBSPHERE_DATABASE_URL);
const asset = await store.addAsset({ name: 'PG18 alert event smoke', host: 'localhost', port: 5432,
  database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'k8s',
  k8sCluster: 'smoke', namespace: 'database', pod: 'pg-0' }, 'encrypted-test-placeholder');
const k8s = (oom = false, usage = null) => ({
  status: 'ok', pod: { name: 'pg-0', uid: 'pod-smoke', conditions: [{ type: 'Ready', status: oom ? 'False' : 'True' }] },
  containers: [{ name: 'postgres', restartCount: oom ? 1 : 0,
    terminatedReason: oom ? 'OOMKilled' : null,
    lastTerminatedAt: oom ? new Date().toISOString() : null }],
  pvcs: usage == null ? [] : [{ name: 'data', capacity: '1Gi', usage: { status: 'ok', usedBytes: usage } }],
  node: { name: 'worker-1', conditions: [{ type: 'MemoryPressure', status: 'False' }] }
});
try {
  await store.updateAlertRule('collector_stale', { threshold: 2 });
  await store.recordResult(asset.id, { k8s: k8s(true) }, 'connection refused');
  const outage = (await store.listEvents()).filter(event => event.assetId === asset.id);
  assert.equal(outage.length, 1);
  const eventId = outage[0].id;
  const evidence = (await store.getEvent(eventId)).evidence;
  assert.deepEqual(new Set(evidence.filter(item => item.kind === 'alert_firing').map(item => item.payload.ruleId)),
    new Set(['pg_unavailable', 'k8s_oom_killed']));
  await store.acknowledgeEvent(eventId);
  await store.acknowledgeEvent(eventId);
  assert.equal((await store.getEvent(eventId)).evidence.filter(item => item.kind === 'event_acknowledged').length, 1);
  await store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100, k8s: k8s(false) });
  assert.equal((await store.getEvent(eventId)).status, 'resolved');

  await store.updateAlertRule('k8s_pvc_high', { durationSeconds: 0 });
  await store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100,
    k8s: k8s(false, 1024 ** 3 * 0.9) });
  const storage = (await store.listEvents()).find(event => event.assetId === asset.id && event.eventGroup === 'storage');
  assert.ok(storage);
  await store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100,
    k8s: { status: 'error', reason: 'Kubernetes API forbidden' } });
  assert.equal((await store.getEvent(storage.id)).status, 'open');
  assert.equal((await store.getEvent(storage.id)).evidence.filter(item => item.kind === 'alert_signal_missing').length, 1);
  await store.pool.query("UPDATE assets SET last_sample_at=now()-interval '3 minutes' WHERE id=$1", [asset.id]);
  await store.checkStale(asset.id);
  const stale = (await store.listActiveAlerts()).find(item => item.assetId === asset.id && item.ruleId === 'collector_stale');
  assert.ok(stale);
  await store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100, k8s: k8s(false, 1024 ** 3 * 0.9) });
  assert.equal((await store.getEvent(stale.state.eventId)).status, 'resolved');
  console.log('PostgreSQL 18 alert events coalesce related faults and keep unrelated or unknown signals separate');
} finally { await store.close(); }
