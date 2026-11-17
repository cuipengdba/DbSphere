import test from 'node:test';
import assert from 'node:assert/strict';
import { sanitizePlan, comparePlans } from '../src/sql-plans.js';

test('imported EXPLAIN plan keeps numeric evidence and discards SQL expressions', () => {
  const plan = sanitizePlan([{ Plan: { 'Node Type': 'Nested Loop', 'Total Cost': 100,
    'Plan Rows': 2, 'Actual Total Time': 250, Filter: "password = 'secret'",
    Plans: [{ 'Node Type': 'Seq Scan', 'Relation Name': 'private_users',
      'Actual Total Time': 100, 'Shared Read Blocks': 12 }] },
  'Execution Time': 260 }], '123');
  assert.equal(plan.summary.nodeCount, 2);
  assert.equal(plan.summary.nodes[1].sharedReadBlocks, 12);
  assert.equal(JSON.stringify(plan).includes('secret'), false);
  assert.equal(JSON.stringify(plan).includes('private_users'), false);
  const newer = sanitizePlan([{ Plan: { 'Node Type': 'Hash Join' },
    'Execution Time': 520 }], '123');
  const [comparison] = comparePlans([newer, plan]);
  assert.equal(comparison.comparison.shapeChanged, true);
  assert.equal(comparison.comparison.executionRatio, 2);
  assert.throws(() => sanitizePlan([{ Plan: { 'Node Type': 'Seq Scan' } }], 'bad'));
});
