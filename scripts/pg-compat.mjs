import assert from 'node:assert/strict';
import pg from 'pg';
import { collectPostgres } from '../src/postgres.js';

const major = Number(process.env.DBSPHERE_TEST_PG_MAJOR);
if (!Number.isInteger(major)) throw new Error('Set DBSPHERE_TEST_PG_MAJOR');
const metrics = await collectPostgres({
  host: process.env.DBSPHERE_TEST_PG_HOST || '127.0.0.1',
  port: Number(process.env.DBSPHERE_TEST_PG_PORT || 15432),
  database_name: 'postgres',
  username: 'dbsphere_monitor',
  ssl_mode: 'disable'
}, process.env.DBSPHERE_TEST_PG_PASSWORD || 'dbsphere-monitor-test');
assert.equal(Math.floor(metrics.version_num / 10000), major);
assert.equal(metrics.pg14_eol, major === 14);
assert.equal(metrics.role, 'primary');
assert.equal(metrics.monitor_role, true);
assert.equal(metrics.privileged_role, false);
assert.ok(metrics.connections >= 1);
assert.ok(metrics.database_bytes > 0);
assert.ok(metrics.replicas !== null);
for (const name of ['activity', 'transactions', 'locks', 'lockChain', 'database', 'wal', 'archiver', 'checkpoints', 'vacuum', 'tables', 'slots']) {
  assert.equal(metrics.diagnostics[name].status, 'ok', `${name}: ${metrics.diagnostics[name].reason}`);
}
console.log(`PG${major} compatible: ${Object.keys(metrics.diagnostics).length} diagnostic sections available`);

const connection = {
  host: process.env.DBSPHERE_TEST_PG_HOST || '127.0.0.1',
  port: Number(process.env.DBSPHERE_TEST_PG_PORT || 15432),
  database: 'postgres', user: 'postgres', password: 'dbsphere-root-test',
  query_timeout: 12000
};
const blocker = new pg.Client(connection);
const waiter = new pg.Client(connection);
await blocker.connect();
await waiter.connect();
let waiting;
try {
  const blockerPid = (await blocker.query('SELECT pg_backend_pid() AS pid')).rows[0].pid;
  const waitingPid = (await waiter.query('SELECT pg_backend_pid() AS pid')).rows[0].pid;
  await blocker.query('SELECT pg_advisory_lock(1742026)');
  waiting = waiter.query('SELECT pg_advisory_lock(1742026)');
  let found = false;
  for (let attempt = 0; attempt < 10; attempt++) {
    await new Promise(resolve => setTimeout(resolve, 100));
    const sample = await collectPostgres({
      host: connection.host, port: connection.port, database_name: 'postgres',
      username: 'dbsphere_monitor', ssl_mode: 'disable'
    }, process.env.DBSPHERE_TEST_PG_PASSWORD || 'dbsphere-monitor-test');
    const chain = sample.diagnostics.lockChain.data;
    if (sample.diagnostics.locks.data.blockedSessions > 0 &&
        chain.some(edge => edge.waitingPid === waitingPid && edge.blockerPid === blockerPid)) {
      found = true;
      break;
    }
  }
  assert.ok(found, `PG${major} failed to expose the advisory lock blocking chain`);
  console.log(`PG${major} lock chain: blocker ${blockerPid} -> waiter ${waitingPid}`);
} finally {
  await blocker.query('SELECT pg_advisory_unlock(1742026)').catch(() => {});
  await waiting?.catch(() => {});
  await blocker.end();
  await waiter.end();
}
