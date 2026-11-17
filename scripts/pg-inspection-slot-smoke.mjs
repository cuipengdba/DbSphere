import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import pg from 'pg';
import { collectPostgres } from '../src/postgres.js';
import { DEFAULT_INSPECTION_TEMPLATES, evaluateInspection } from '../src/inspections.js';

if (!process.env.DBSPHERE_DATABASE_URL) throw new Error('Set DBSPHERE_DATABASE_URL');
const url = new URL(process.env.DBSPHERE_DATABASE_URL);
const client = new pg.Client({ connectionString: url.toString() });
const slotName = `dbsphere_inspect_${randomUUID().replaceAll('-', '').slice(0, 12)}`;
await client.connect();
let created = false;
try {
  await client.query('SELECT pg_create_physical_replication_slot($1, true)', [slotName]);
  created = true;
  await client.query('SELECT pg_switch_wal()');
  const metrics = await collectPostgres({ host: url.hostname, port: Number(url.port || 5432),
    database_name: url.pathname.slice(1), username: decodeURIComponent(url.username),
    ssl_mode: 'disable' }, decodeURIComponent(url.password));
  const slot = metrics.diagnostics.slotDetails.data.find(item => item.name === slotName);
  assert.ok(slot, 'New physical replication slot was not visible to the monitor');
  assert.ok(slot.retainedBytes > 0, 'Replication slot should retain WAL after a switch');
  const builtin = DEFAULT_INSPECTION_TEMPLATES.find(item => item.id === 'pg_daily');
  const template = { ...builtin, checks: builtin.checks.map(item => item.id === 'slot_retention'
    ? { ...item, threshold: 1 } : item) };
  const result = evaluateInspection(template, { runtime: 'linux' }, { status: 'ok',
    collectedAt: new Date().toISOString(), metrics }, null);
  assert.equal(result.findings.find(item => item.id === 'slot_retention').status, 'risk');
  console.log('Real PG18 replication slot WAL retention is visible to inspection');
} finally {
  if (created) await client.query('SELECT pg_drop_replication_slot($1)', [slotName]);
  await client.end();
}
