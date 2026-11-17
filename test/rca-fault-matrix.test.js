import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { Store } from '../src/store.js';
import { analyzeEvent, eventSampleIds } from '../src/rca.js';

test('injected fault matrix keeps each RCA category on its own event with source evidence', t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const directory = mkdtempSync(join(root, 'dbsphere-rca-matrix-'));
  const store = new Store(directory);
  t.after(() => {
    store.close();
    if (!resolve(directory).startsWith(root + sep)) throw new Error('Unexpected test directory');
    rmSync(directory, { recursive: true, force: true });
  });
  for (const id of ['pg_connections_high', 'pg_lock_wait', 'pg_replication_lag',
    'linux_disk_high', 'k8s_pvc_high', 'k8s_node_pressure', 'k8s_pod_unscheduled']) {
    store.updateAlertRule(id, { durationSeconds: 0 });
  }
  const k8s = (extra = {}) => ({ status: 'ok', pod: { uid: 'pod-1', name: 'pg-0',
    conditions: [{ type: 'Ready', status: 'True' }, { type: 'PodScheduled', status: 'True' }] },
  containers: [], pvcs: [], events: [], ...extra });
  const scenarios = [
    { name: 'connections', runtime: 'linux', result: { connections: 90, max_connections: 100 },
      candidate: 'connection_pressure' },
    { name: 'lock', runtime: 'linux', result: { diagnostics: {
      locks: { status: 'ok', data: { blockedSessions: 1 } },
      lockChain: { status: 'ok', data: [{ waitingPid: 21, blockerPid: 10 }] }
    } }, candidate: 'lock_chain' },
    { name: 'replication', runtime: 'linux', result: { role: 'primary', replication_observed: true,
      replicas: 1, replication_lag_bytes: 128 * 1024 ** 2 }, candidate: 'replication_lag' },
    { name: 'archive', runtime: 'linux', prior: { diagnostics: {
      archiver: { status: 'ok', data: { failedCount: 0 } }
    } }, result: { diagnostics: { archiver: { status: 'ok', data: { failedCount: 1 } } } },
    candidate: 'wal_archive_failures' },
    { name: 'autovacuum', runtime: 'linux', result: { diagnostics: {
      settings: { status: 'ok', data: [{ name: 'autovacuum', value: 'off' }] },
      vacuum: { status: 'ok', data: [{ schema: 'public', table: 'orders',
        liveRowsEstimate: 30000, deadRowsEstimate: 20000 }] }
    } }, candidate: 'autovacuum_disabled' },
    { name: 'linux-disk', runtime: 'linux', result: { linux: { status: 'ok',
      filesystems: [{ mountpoint: '/var/lib/postgresql', usedPercent: 95 }] } },
    candidate: 'linux_disk_pressure' },
    { name: 'oom', runtime: 'k8s', error: 'connect ECONNREFUSED 127.0.0.1:5432',
      result: { k8s: k8s({ containers: [{ name: 'postgres', terminatedReason: 'OOMKilled',
        lastTerminatedAt: new Date(Date.now() - 30_000).toISOString(), restartCount: 1 }] }) },
      candidate: 'pod_oom_correlated' },
    { name: 'pvc', runtime: 'k8s', result: { k8s: k8s({ pvcs: [{ name: 'data', capacity: '1Gi',
      usage: { status: 'ok', usedBytes: 0.9 * 1024 ** 3 } }] }) }, candidate: 'pvc_pressure' },
    { name: 'node-pressure', runtime: 'k8s', result: { k8s: k8s({ node: { name: 'worker-1',
      conditions: [{ type: 'MemoryPressure', status: 'True' }] } }) }, candidate: 'k8s_node_pressure' },
    { name: 'scheduling', runtime: 'k8s', error: 'connect ECONNREFUSED 127.0.0.1:5432',
      result: { k8s: k8s({ pod: { uid: 'pod-2', name: 'pg-1', conditions: [
        { type: 'PodScheduled', status: 'False', reason: 'Unschedulable' }] },
      events: [{ reason: 'FailedScheduling', at: new Date(Date.now() - 30_000).toISOString() }] }) },
      candidate: 'k8s_scheduling' }
  ];
  const eventIds = new Set();
  for (const scenario of scenarios) {
    const asset = store.addAsset({ name: scenario.name, host: '127.0.0.1', port: 5432,
      database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: scenario.runtime,
      ...(scenario.runtime === 'k8s' ? { k8sCluster: 'test', namespace: 'db', pod: 'pg-0' } : {}) },
    'test-placeholder');
    if (scenario.prior) store.recordResult(asset.id, scenario.prior);
    store.recordResult(asset.id, scenario.result, scenario.error ?? null);
    const summaries = store.listEvents().filter(event => event.assetId === asset.id);
    assert.ok(summaries.length >= 1, scenario.name);
    const matching = summaries.map(summary => store.getEvent(summary.id)).find(event => {
      const samples = store.getSamplesByIds(asset.id, eventSampleIds(event));
      return analyzeEvent(event, samples, { nearbySamples: samples }).candidates
        .some(candidate => candidate.code === scenario.candidate);
    });
    assert.ok(matching, `${scenario.name} missing ${scenario.candidate}`);
    assert.ok(matching.evidence.some(item => item.kind === 'alert_firing' ||
      item.kind === 'root_cause_candidate'), scenario.name);
    assert.ok(!eventIds.has(matching.id), `${scenario.name} reused another asset's event`);
    eventIds.add(matching.id);
  }
  assert.equal(eventIds.size, scenarios.length);
});
