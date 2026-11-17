import test from 'node:test';
import assert from 'node:assert/strict';
import { analyzeEvent, eventSampleIds } from '../src/rca.js';

const eventId = '11111111-1111-4111-8111-111111111111';
const sampleId = '22222222-2222-4222-8222-222222222222';
const evidenceId = '33333333-3333-4333-8333-333333333333';
const base = { id: eventId, assetId: '44444444-4444-4444-8444-444444444444',
  assetName: 'pg18', title: '锁等待', startedAt: '2026-11-17T00:00:00Z', evidence: [] };

test('lock evidence stays under one incident and exposes blocking pair', () => {
  const event = { ...base, evidence: [{ id: evidenceId, kind: 'alert_firing',
    observedAt: '2026-11-17T00:01:00Z', payload: { metric: 'pg.blocked_sessions', sampleId } }] };
  const sample = { id: sampleId, collectedAt: '2026-11-17T00:01:00Z', status: 'ok',
    metrics: { diagnostics: { lockChain: { status: 'ok', data: [
      { waitingPid: 21, blockerPid: 10, waitingSince: '2026-11-17T00:00:30Z' }
    ] } }, k8s: { status: 'ok', pod: { uid: 'pod-1', name: 'pg-0' },
      node: { name: 'worker-1' }, pvcs: [{ name: 'pg-data' }] } } };
  assert.deepEqual(eventSampleIds(event), [sampleId]);
  const analysis = analyzeEvent(event, [sample]);
  assert.equal(analysis.eventId, eventId);
  assert.equal(analysis.status, 'candidates');
  assert.equal(analysis.candidates[0].code, 'lock_chain');
  assert.deepEqual(analysis.candidates[0].detail.pairs[0],
    { waitingPid: 21, blockerPid: 10, waitingSince: '2026-11-17T00:00:30Z' });
  assert.ok(analysis.graph.edges.some(edge => edge.from === `evidence:${evidenceId}` &&
    edge.to === `sample:${sampleId}`));
  assert.ok(analysis.graph.nodes.some(node => node.id === 'pod:pod-1'));
});

test('alert without diagnostic proof remains unlocated', () => {
  const event = { ...base, evidence: [{ id: evidenceId, kind: 'alert_firing',
    observedAt: '2026-11-17T00:01:00Z', payload: { metric: 'pg.blocked_sessions', sampleId } }] };
  const analysis = analyzeEvent(event, [{ id: sampleId, status: 'ok', metrics: {
    diagnostics: { lockChain: { status: 'unavailable', reason: 'permission denied' } }
  } }]);
  assert.equal(analysis.status, 'unlocated');
  assert.equal(analysis.candidates.length, 0);
  assert.ok(analysis.missingSignals.some(signal => signal.includes('锁链')));
});
