import assert from 'node:assert/strict';
import pg from 'pg';
import { collectPostgres } from '../src/postgres.js';
import { sanitizePlan } from '../src/sql-plans.js';

const major = Number(process.env.DBSPHERE_TEST_PG_MAJOR || 18);
const client = new pg.Client({ host: '127.0.0.1', port: 15432, database: 'postgres',
  user: 'dbsphere_monitor', password: 'dbsphere-monitor-test' });
await client.connect();
try {
  for (let i = 0; i < 8; i++) await client.query('SELECT $1::int + 1', [i]);
  const metrics = await collectPostgres({ host: '127.0.0.1', port: 15432,
    database_name: 'postgres', username: 'dbsphere_monitor', ssl_mode: 'disable' },
  'dbsphere-monitor-test');
  assert.equal(Math.floor(metrics.version_num / 10000), major);
  if (process.env.DBSPHERE_SQL_EXPECTED === 'not_configured') {
    assert.equal(metrics.sqlPerformance.status, 'not_configured');
    console.log(`PG${major} without pg_stat_statements degrades gracefully`);
  } else {
    assert.equal(metrics.sqlPerformance.status, 'ok', JSON.stringify(metrics.sqlPerformance));
    assert.ok(metrics.sqlPerformance.statements.length > 0);
    assert.ok(metrics.sqlPerformance.statements.every(row => row.queryId &&
      Number.isFinite(row.totalExecMs) && Number.isFinite(row.calls)));
    assert.ok(metrics.sqlPerformance.resetAt);
    assert.equal(JSON.stringify(metrics.sqlPerformance).includes('SELECT'), false,
      'The persistent SQL snapshot must not contain query text');
    const explain = (await client.query('EXPLAIN (FORMAT JSON) SELECT 1')).rows[0]['QUERY PLAN'];
    const plan = sanitizePlan(explain, metrics.sqlPerformance.statements[0].queryId);
    assert.ok(plan.summary.nodeCount > 0);
    assert.equal(JSON.stringify(plan).includes('SELECT'), false);
    console.log(`PG${major} pg_stat_statements collection passed`);
  }
} finally { await client.end(); }
