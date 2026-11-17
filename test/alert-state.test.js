import test from 'node:test';
import assert from 'node:assert/strict';
import { nextAlertState } from '../src/alert-state.js';

const rule = { operator: '>=', threshold: 90, durationSeconds: 60 };
const signal = value => ({ status: 'ok', value });

test('an alert fires only after sustained breach, keeps its event, and recovers on observed normal data', () => {
  const start = '2026-11-17T00:00:00.000Z';
  const pending = nextAlertState(rule, signal(95), null, start);
  assert.equal(pending.next.status, 'pending');
  assert.equal(pending.transition, null);
  const early = nextAlertState(rule, signal(93), pending.next, '2026-11-17T00:00:59.000Z');
  assert.equal(early.next.status, 'pending');
  const fired = nextAlertState(rule, signal(94), early.next, '2026-11-17T00:01:00.000Z');
  assert.equal(fired.next.status, 'firing');
  assert.equal(fired.transition, 'firing');
  const active = nextAlertState(rule, signal(91), { ...fired.next, eventId: 'event-1' }, '2026-11-17T00:02:00.000Z');
  assert.equal(active.next.eventId, 'event-1');
  assert.equal(active.transition, null);
  const recovered = nextAlertState(rule, signal(60), active.next, '2026-11-17T00:03:00.000Z');
  assert.equal(recovered.next, null);
  assert.equal(recovered.transition, 'recovered');
});

test('a missing signal cannot clear a firing alert or complete a pending duration', () => {
  const fired = { status: 'firing', pendingSince: '2026-11-17T00:00:00Z',
    firedAt: '2026-11-17T00:01:00Z', eventId: 'event-1' };
  const missing = nextAlertState(rule, { status: 'unavailable', value: null }, fired, '2026-11-17T00:02:00Z');
  assert.equal(missing.next.status, 'firing');
  assert.equal(missing.next.eventId, 'event-1');
  assert.equal(missing.transition, 'signal_missing');
  assert.equal(nextAlertState(rule, { status: 'unavailable', value: null }, missing.next, '2026-11-17T00:03:00Z').transition, null);
  const pending = { status: 'pending', pendingSince: '2026-11-17T00:00:00Z' };
  assert.equal(nextAlertState(rule, { status: 'unavailable', value: null }, pending, '2026-11-17T00:03:00Z').next, null);
});
