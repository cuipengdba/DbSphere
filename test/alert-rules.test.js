import test from 'node:test';
import assert from 'node:assert/strict';
import { DEFAULT_RULES, RULE_PACK_VERSION, compareSignal, readSignal, validateRuleUpdate } from '../src/alert-rules.js';

test('default PostgreSQL, Linux, Kubernetes and PgBouncer rules have stable identities', () => {
  assert.equal(new Set(DEFAULT_RULES.map(rule => rule.id)).size, DEFAULT_RULES.length);
  assert.ok(DEFAULT_RULES.every(rule => rule.version === RULE_PACK_VERSION && rule.enabled));
  assert.ok(['PostgreSQL', 'node_exporter', 'Kubernetes API', 'Prometheus kubelet', 'PgBouncer']
    .every(source => DEFAULT_RULES.some(rule => rule.source === source)));
});

test('PostgreSQL connection failures are distinct from credential and permission failures', () => {
  const down = readSignal('pg.unavailable', {}, null, 'connect ECONNREFUSED 127.0.0.1:5432');
  assert.equal(compareSignal(down, DEFAULT_RULES.find(rule => rule.id === 'pg_unavailable')), 'breach');
  assert.equal(readSignal('pg.unavailable', {}, null, 'password authentication failed').status, 'unavailable');
  assert.equal(readSignal('collector.pg_failed', {}, null, 'password authentication failed').value, 1);
  assert.equal(readSignal('pg.unavailable', { connections: 2 }, null, null).value, 0);
});

test('missing cross-layer signals stay unknown and cannot clear an alert', () => {
  const rule = DEFAULT_RULES.find(item => item.id === 'linux_disk_high');
  assert.equal(compareSignal(readSignal(rule.metric, { linux: { status: 'not_configured' } }), rule), 'unknown');
  assert.equal(readSignal('k8s.pvc_used_percent', { k8s: { status: 'ok', pvcs: [
    { name: 'data', capacity: '1Gi', usage: { status: 'not_configured' } }
  ] } }).status, 'unavailable');
  assert.equal(readSignal('pg.blocked_sessions', { diagnostics: { locks: { status: 'unavailable' } } }).status, 'unavailable');
});

test('configured Linux and Kubernetes collection failures alert without declaring targets healthy', () => {
  assert.equal(readSignal('collector.k8s_failed', { k8s: { status: 'error', reason: '403 Forbidden' } }).value, 1);
  assert.equal(readSignal('collector.linux_failed', { linux: { status: 'error', reason: 'connection refused' } }).value, 1);
  assert.equal(readSignal('collector.k8s_failed', { k8s: { status: 'not_configured' } }).status, 'unavailable');
  assert.equal(readSignal('collector.linux_failed', { linux: { status: 'not_configured' } }).status, 'unavailable');
  assert.equal(readSignal('collector.k8s_failed', { k8s: { status: 'ok' } }).value, 0);
});

test('rule signals use real metric shape and same-Pod restart deltas', () => {
  const now = Date.parse('2026-11-17T00:00:00Z');
  const previous = { k8s: { status: 'ok', pod: { uid: 'pod-1' }, containers: [{ name: 'postgres', restartCount: 1 }] } };
  const result = {
    role: 'primary', replication_observed: true, replicas: 1, replication_lag_bytes: 128 * 1024 ** 2,
    connections: 90, max_connections: 100,
    diagnostics: { locks: { status: 'ok', data: { blockedSessions: 2 } }, transactions: { status: 'ok', data: { longTransactions: 1 } },
      archiver: { status: 'ok', data: { failedCount: 3 } } },
    linux: { status: 'ok', cpu: { usedPercent: 92 }, memory: { usedPercent: 91 }, filesystems: [
      { mountpoint: '/var/lib/postgresql', usedPercent: 95 }, { mountpoint: '/', usedPercent: 40 }
    ] },
    k8s: { status: 'ok', pod: { name: 'pg-0', uid: 'pod-1', conditions: [{ type: 'Ready', status: 'False' }] },
      containers: [{ name: 'postgres', restartCount: 2, terminatedReason: 'OOMKilled', lastTerminatedAt: new Date(now - 30000).toISOString() }],
      node: { name: 'worker-1', conditions: [{ type: 'MemoryPressure', status: 'True' }] },
      pvcs: [{ name: 'data', capacity: '1Gi', usage: { status: 'ok', usedBytes: 1024 ** 3 * 0.9 } }] },
    pgbouncer: { status: 'ok', pools: [{ clientWaiting: 2 }] }
  };
  assert.equal(readSignal('pg.connection_utilization', result).value, 90);
  assert.equal(readSignal('pg.blocked_sessions', result).value, 2);
  assert.equal(readSignal('pg.replication_lag_bytes', result).value, 128 * 1024 ** 2);
  assert.equal(readSignal('pg.wal_receiver_down', { role: 'standby', replication_observed: true, wal_receiver: null }).value, 1);
  assert.equal(readSignal('pg.archive_fail_delta', result,
    { diagnostics: { archiver: { status: 'ok', data: { failedCount: 2 } } } }).value, 1);
  assert.equal(readSignal('pg.archive_fail_delta', result).status, 'unavailable');
  assert.equal(readSignal('linux.disk_used_percent', result).detail.mountpoint, '/var/lib/postgresql');
  assert.equal(readSignal('k8s.pod_not_ready', result).value, 1);
  assert.equal(readSignal('k8s.restart_delta', result, previous).value, 1);
  assert.equal(readSignal('k8s.oom_recent', result, previous, null, now).value, 1);
  assert.equal(readSignal('k8s.node_pressure', result).value, 1);
  assert.equal(readSignal('k8s.pvc_used_percent', result).value, 90);
  assert.equal(readSignal('pgbouncer.waiting_clients', result).value, 2);
  assert.equal(readSignal('k8s.restart_delta', result, { k8s: { ...previous.k8s, pod: { uid: 'other-pod' } } }).status, 'unavailable');
});

test('rule edits are bounded and cannot change metric identity', () => {
  const rule = DEFAULT_RULES[0];
  assert.equal(validateRuleUpdate(rule, { threshold: 2, durationSeconds: 60, severity: 'warning' }), true);
  assert.equal(validateRuleUpdate(rule, { metric: 'pg.admin_sql' }), false);
  assert.equal(validateRuleUpdate(rule, { threshold: Number.POSITIVE_INFINITY }), false);
  assert.equal(validateRuleUpdate(rule, { durationSeconds: -1 }), false);
});
