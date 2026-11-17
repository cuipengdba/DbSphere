import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import pg from 'pg';
import { collectPostgres } from '../src/postgres.js';
import { sqlPerformanceSummary } from '../src/sql-trends.js';

const client = new pg.Client({ host: '127.0.0.1', port: 15432, database: 'postgres',
  user: 'dbsphere_monitor', password: 'dbsphere-monitor-test' });
await client.connect();
const collect = async () => ({ id: randomUUID(), status: 'ok',
  collectedAt: new Date().toISOString(), metrics: await collectPostgres({
    host: '127.0.0.1', port: 15432, database_name: 'postgres',
    username: 'dbsphere_monitor', ssl_mode: 'disable'
  }, 'dbsphere-monitor-test') });
try {
  await client.query('SELECT pg_sleep($1::double precision)', [0.01]);
  const oldest = await collect();
  for (let i = 0; i < 5; i++) await client.query('SELECT pg_sleep($1::double precision)', [0.01]);
  const middle = await collect();
  for (let i = 0; i < 5; i++) await client.query('SELECT pg_sleep($1::double precision)', [0.09]);
  const latest = await collect();
  const summary = sqlPerformanceSummary([latest, middle, oldest]);
  const regressed = summary.statements.find(row => row.regression && row.delta.calls === 5);
  assert.ok(regressed, 'Real SQL latency regression should be visible from two sampling intervals');
  assert.ok(regressed.regression.ratio >= 2);
  console.log(`Real PG18 SQL regression: Query ID ${regressed.queryId}, ratio ${regressed.regression.ratio.toFixed(1)}`);
} finally { await client.end(); }
