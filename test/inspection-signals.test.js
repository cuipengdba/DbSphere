import test from 'node:test';
import assert from 'node:assert/strict';
import { DEFAULT_INSPECTION_TEMPLATES, evaluateInspection, upgradeInspectionTemplate } from '../src/inspections.js';

const template = id => DEFAULT_INSPECTION_TEMPLATES.find(item => item.id === id);
const at = '2026-11-17T08:00:00Z';

test('PostgreSQL inspection checks freeze, slots, permissions and role-specific replication', () => {
  const pg = template('pg_daily');
  const asset = { runtime: 'linux' };
  const metrics = { version_num: 180006, monitor_role: true, privileged_role: true,
    role: 'primary', replication_observed: true, replicas: 0,
    connections: 10, max_connections: 100,
    diagnostics: { freeze: { status: 'ok', data: { usedPercent: 90 } },
      slotDetails: { status: 'ok', data: [{ name: 'stale', active: false,
        walStatus: 'lost', retainedBytes: 11 * 1024 ** 3 }] },
      settings: { status: 'ok', data: [{ name: 'autovacuum', value: 'off' }] } } };
  const result = evaluateInspection(pg, asset, { status: 'ok', collectedAt: at, metrics }, null);
  for (const id of ['freeze_age', 'inactive_slots', 'slot_retention', 'lost_slots',
    'privileged_monitor', 'autovacuum_disabled']) {
    assert.equal(result.findings.find(item => item.id === id).status, 'risk', id);
  }
  assert.equal(result.findings.find(item => item.id === 'wal_receiver').status, 'not_applicable');
  assert.equal(result.findings.find(item => item.id === 'replication_lag').status, 'not_applicable');
  const standby = evaluateInspection(pg, asset, { status: 'ok', collectedAt: at,
    metrics: { ...metrics, role: 'standby', wal_receiver: 'stopped' } }, null);
  assert.equal(standby.findings.find(item => item.id === 'wal_receiver').status, 'risk');
  assert.equal(standby.findings.find(item => item.id === 'replication_lag').status, 'not_applicable');
});

test('Linux permission and Kubernetes probe, scheduling and PVC risks keep source evidence', () => {
  const linux = evaluateInspection(template('linux_daily'), { runtime: 'linux' },
    { status: 'ok', collectedAt: at, metrics: { linux: { status: 'error', reason: 'HTTP 403 Forbidden' } } }, null);
  assert.equal(linux.findings.find(item => item.id === 'permissions').status, 'risk');
  const k8s = evaluateInspection(template('k8s_daily'), { runtime: 'k8s' },
    { status: 'ok', collectedAt: at, metrics: { k8s: { status: 'ok',
      pod: { name: 'pg-0', uid: 'pod-1', conditions: [{ type: 'Ready', status: 'False' }] },
      containers: [], node: { name: 'node-1', conditions: [] },
      pvcs: [{ name: 'data', phase: 'Bound', capacity: '1Gi',
        usage: { status: 'ok', usedBytes: 0.9 * 1024 ** 3 } },
      { name: 'other', phase: 'Pending' }],
      events: [{ at, reason: 'Unhealthy' }, { at, reason: 'FailedScheduling' }] } } }, null);
  for (const id of ['pod', 'pvc', 'probe_events', 'scheduling_events', 'unbound_pvc']) {
    assert.equal(k8s.findings.find(item => item.id === id).status, 'risk', id);
  }
  assert.equal(k8s.findings.find(item => item.id === 'probe_events').source, 'Kubernetes Event');
});

test('template catalog migration preserves operator thresholds while adding checks', () => {
  const builtin = template('pg_daily');
  const legacy = { ...builtin, version: 4, catalogVersion: 1,
    checks: builtin.checks.slice(0, 7).map(item => ({ ...item,
      threshold: item.id === 'connections' ? 75 : item.threshold })) };
  const updated = upgradeInspectionTemplate(legacy, builtin);
  assert.equal(updated.version, 5);
  assert.equal(updated.checks.find(item => item.id === 'connections').threshold, 75);
  assert.ok(updated.checks.some(item => item.id === 'wal_receiver'));
  assert.equal(upgradeInspectionTemplate(updated, builtin), null);
});
