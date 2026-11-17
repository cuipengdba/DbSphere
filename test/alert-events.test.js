import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { Store } from '../src/store.js';
import { normalizeSilence } from '../src/silences.js';

function withStore(t) {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const directory = mkdtempSync(join(root, 'dbsphere-alert-events-'));
  const store = new Store(directory);
  t.after(() => {
    store.close();
    if (!resolve(directory).startsWith(root + sep)) throw new Error('Refusing to remove data outside the test root');
    rmSync(directory, { recursive: true, force: true });
  });
  const asset = store.addAsset({ name: 'pg-k8s', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'k8s',
    k8sCluster: 'test', namespace: 'database', pod: 'pg-0' }, 'encrypted-placeholder');
  return { store, asset };
}

function k8s(oom = false, pvcPercent = null) {
  return { status: 'ok', pod: { name: 'pg-0', uid: 'pod-1', conditions: [{ type: 'Ready', status: oom ? 'False' : 'True' }] },
    containers: [{ name: 'postgres', restartCount: oom ? 1 : 0,
      terminatedReason: oom ? 'OOMKilled' : null,
      lastTerminatedAt: oom ? new Date().toISOString() : null }],
    pvcs: pvcPercent == null ? [] : [{ name: 'data', capacity: '1Gi', usage: {
      status: 'ok', usedBytes: 1024 ** 3 * pvcPercent / 100
    } }], node: { name: 'worker-1', conditions: [{ type: 'MemoryPressure', status: 'False' }] } };
}

test('one PostgreSQL outage and same-Pod OOM share an event without duplicate alert transitions', t => {
  const { store, asset } = withStore(t);
  const failure = { k8s: k8s(true) };
  store.recordResult(asset.id, failure, 'connection refused');
  assert.equal(store.listEvents().length, 1);
  const eventId = store.listEvents()[0].id;
  let event = store.getEvent(eventId);
  assert.equal(event.evidence.filter(item => item.kind === 'root_cause_candidate').length, 1);
  assert.deepEqual(new Set(event.evidence.filter(item => item.kind === 'alert_firing').map(item => item.payload.ruleId)),
    new Set(['pg_unavailable', 'k8s_oom_killed']));
  store.recordResult(asset.id, failure, 'connection refused');
  event = store.getEvent(eventId);
  assert.equal(event.evidence.filter(item => item.kind === 'alert_firing').length, 2);
  assert.equal(store.listEvents().length, 1);
  store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100, k8s: k8s(false) });
  event = store.getEvent(eventId);
  assert.equal(event.status, 'resolved');
  assert.equal(event.evidence.filter(item => item.kind === 'alert_recovered').length, 2);
  assert.equal(store.listActiveAlerts().length, 0);
});

test('an unrelated PVC alert gets its own event and missing usage cannot clear it', t => {
  const { store, asset } = withStore(t);
  store.updateAlertRule('k8s_pvc_high', { durationSeconds: 0 });
  store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100, k8s: k8s(false, 90) });
  const storageEvent = store.listEvents().find(item => item.title.includes('PVC'));
  assert.ok(storageEvent);
  store.recordResult(asset.id, { k8s: k8s(false, 90) }, 'connection refused');
  const events = store.listEvents();
  assert.equal(events.length, 2);
  assert.ok(events.some(item => item.id === storageEvent.id && item.status === 'open'));
  assert.ok(events.some(item => item.eventGroup === 'availability' && item.id !== storageEvent.id));
  store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100,
    k8s: { status: 'error', reason: 'Kubernetes permission denied' } });
  assert.equal(store.getEvent(storageEvent.id).status, 'open');
  assert.equal(store.getEvent(storageEvent.id).evidence.filter(item => item.kind === 'alert_signal_missing').length, 1);
});

test('collector staleness opens a telemetry event and only a new sample clears it', t => {
  const { store, asset } = withStore(t);
  store.db.prepare('UPDATE assets SET last_sample_at=? WHERE id=?')
    .run(new Date(Date.now() - 3 * 60 * 1000).toISOString(), asset.id);
  store.checkStale(asset.id);
  const alert = store.listActiveAlerts().find(item => item.ruleId === 'collector_stale');
  assert.ok(alert);
  const eventId = alert.state.eventId;
  store.checkStale(asset.id);
  assert.equal(store.getEvent(eventId).evidence.filter(item => item.kind === 'alert_firing').length, 1);
  store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100, k8s: k8s(false) });
  assert.equal(store.getEvent(eventId).status, 'resolved');
  assert.equal(store.getEvent(eventId).evidence.filter(item => item.kind === 'alert_recovered').length, 1);
});

test('maintenance silences notifications while keeping an inspectable event and reactivates on revoke', t => {
  const { store, asset } = withStore(t);
  const silence = store.createSilence(normalizeSilence({ kind: 'maintenance', assetId: asset.id,
    ruleId: 'pg_unavailable', reason: 'planned restart',
    endsAt: new Date(Date.now() + 3600 * 1000).toISOString() }));
  assert.ok(silence);
  store.recordResult(asset.id, { k8s: k8s(false) }, 'connection refused');
  const active = store.listActiveAlerts().find(item => item.ruleId === 'pg_unavailable');
  assert.equal(active.state.mutedBy, silence.id);
  const eventId = active.state.eventId;
  assert.equal(store.getEvent(eventId).evidence.filter(item => item.kind === 'alert_suppressed').length, 1);
  assert.equal(store.getEvent(eventId).status, 'open');
  assert.ok(store.revokeSilence(silence.id).revokedAt);
  store.recordResult(asset.id, { k8s: k8s(false) }, 'connection refused');
  assert.equal(store.listActiveAlerts().find(item => item.ruleId === 'pg_unavailable').state.mutedBy, null);
  assert.equal(store.getEvent(eventId).evidence.filter(item => item.kind === 'alert_unsuppressed').length, 1);
  store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100, k8s: k8s(false) });
  assert.equal(store.getEvent(eventId).status, 'resolved');
});
