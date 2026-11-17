import test from 'node:test';
import assert from 'node:assert/strict';
import { normalizeSilence } from '../src/silences.js';

test('silence windows require an explicit bounded time, scope and reason', () => {
  const now = new Date('2026-11-17T00:00:00Z');
  const valid = normalizeSilence({ kind: 'maintenance', reason: 'planned PG restart',
    endsAt: '2026-11-17T01:00:00Z' }, now);
  assert.equal(valid.startsAt, now.toISOString());
  assert.equal(valid.kind, 'maintenance');
  assert.equal(normalizeSilence({ reason: '', endsAt: '2026-11-17T01:00:00Z' }, now), null);
  assert.equal(normalizeSilence({ reason: 'x', endsAt: '2026-12-19T00:00:00Z' }, now), null);
  assert.equal(normalizeSilence({ reason: 'x', endsAt: '2026-11-17T01:00:00Z', assetId: 'not-a-uuid' }, now), null);
  assert.equal(normalizeSilence({ reason: 'x', endsAt: '2026-11-17T01:00:00Z', command: 'rm' }, now), null);
});
