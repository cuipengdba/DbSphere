import test from 'node:test';
import assert from 'node:assert/strict';
import { collectSqlPerformance } from '../src/sql-performance.js';
import { sqlPerformanceSummary } from '../src/sql-trends.js';

test('SQL collector degrades when pg_stat_statements is absent', async () => {
  const result = await collectSqlPerformance({ query: async () => ({ rows: [] }) });
  assert.equal(result.status, 'not_configured');
});

test('SQL collector keeps bounded counters and never saves query text', async () => {
  const client = { query: async sql => {
    if (sql.includes('FROM pg_extension')) return { rows: [{ schema_name: 'public' }] };
    if (sql.includes('FROM pg_attribute')) return { rows: ['dbid', 'userid', 'queryid', 'toplevel',
      'calls', 'total_exec_time', 'rows', 'shared_blks_hit', 'shared_blks_read',
      'shared_blks_written', 'temp_blks_read', 'temp_blks_written',
      'wal_bytes'].map(attname => ({ attname })) };
    if (sql.includes('FROM "public".pg_stat_statements_info')) return { rows: [{ stats_reset: '2026-11-17T00:00:00Z' }] };
    return { rows: [{ dbid: '1', userid: '2', queryid: '3', toplevel: true,
      calls: '10', total_exec_time: 120, rows: '5', shared_blks_hit: '2',
      shared_blks_read: '1', shared_blks_written: '0', temp_blks_read: '0',
      temp_blks_written: '0', wal_bytes: '1024',
      query: "SELECT secret = 'password'" }] };
  } };
  const result = await collectSqlPerformance(client);
  assert.equal(result.status, 'ok');
  assert.equal(result.statements[0].fingerprint, '1:2:3:1');
  assert.equal(result.statements[0].query, undefined);
  assert.equal(JSON.stringify(result).includes('password'), false);
});

test('SQL trend compares reset-safe intervals and identifies meaningful regression', () => {
  const row = (calls, time) => ({ fingerprint: '1:2:3:1', queryId: '3', calls,
    totalExecMs: time, rows: 0, sharedBlocksHit: 0, sharedBlocksRead: 0,
    sharedBlocksWritten: 0, tempBlocksRead: 0, tempBlocksWritten: 0, walBytes: 0 });
  const sample = (id, calls, time, resetAt = '2026-11-17T00:00:00Z') => ({
    id, status: 'ok', collectedAt: '2026-11-17T01:00:00Z',
    metrics: { sqlPerformance: { status: 'ok', source: 'pg_stat_statements',
      resetAt, statements: [row(calls, time)] } }
  });
  const latest = sample('latest', 20, 1200);
  latest.metrics.linux = { cpu: { usedPercent: 85 }, memory: { usedPercent: 70 } };
  const result = sqlPerformanceSummary([
    latest, sample('middle', 10, 100), sample('oldest', 5, 25)
  ]);
  assert.equal(result.comparison, 'interval');
  assert.equal(result.statements[0].delta.calls, 10);
  assert.equal(result.statements[0].regression.ratio > 2, true);
  assert.equal(result.statements[0].trend.at(-1).linuxCpuPercent, 85);
  const reset = sqlPerformanceSummary([
    sample('latest', 20, 1200, '2026-11-17T01:00:00Z'), sample('middle', 10, 100)
  ]);
  assert.equal(reset.comparison, 'cumulative');
  assert.equal(reset.statements[0].delta, null);
});
