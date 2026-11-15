import assert from 'node:assert/strict';
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
console.log(`PG${major} compatible: ${JSON.stringify(metrics)}`);
