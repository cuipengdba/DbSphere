import assert from 'node:assert/strict';
import { PgStore } from '../src/pg-store.js';

if (!process.env.DBSPHERE_DATABASE_URL) throw new Error('Set DBSPHERE_DATABASE_URL');
let store = await PgStore.create(process.env.DBSPHERE_DATABASE_URL);
const before = (await store.getAlertRules()).find(rule => rule.id === 'linux_cpu_high');
assert.ok(before);
const changed = await store.updateAlertRule(before.id, { threshold: 91, durationSeconds: 120 });
assert.equal(changed.version, before.version + 1);
assert.equal(await store.updateAlertRule(before.id, { metric: 'unsafe' }), false);
assert.equal(await store.updateAlertRule('unknown_rule', { threshold: 91 }), null);
await store.close();

store = await PgStore.create(process.env.DBSPHERE_DATABASE_URL);
try {
  const persisted = (await store.getAlertRules()).find(rule => rule.id === before.id);
  assert.equal(persisted.threshold, 91);
  assert.equal(persisted.durationSeconds, 120);
  assert.equal(persisted.version, before.version + 1);
  console.log('PostgreSQL 18 alert rules persist and reject unsafe edits');
} finally { await store.close(); }
