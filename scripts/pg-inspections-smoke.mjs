import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { PgStore } from '../src/pg-store.js';
import { evaluateInspection } from '../src/inspections.js';
import { forecastDatabaseGrowth } from '../src/capacity.js';

if (!process.env.DBSPHERE_DATABASE_URL) throw new Error('Set DBSPHERE_DATABASE_URL');
let store = await PgStore.create(process.env.DBSPHERE_DATABASE_URL);
let asset;
try {
  assert.equal((await store.getInspectionTemplates()).length, 4);
  const original = await store.getInspectionTemplate('pg_daily');
  const changed = await store.updateInspectionTemplate('pg_daily', { thresholds: { connections: 80 } });
  assert.equal(changed.version, original.version + 1);
  asset = await store.addAsset({ name: 'PG18 inspection smoke', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted-placeholder');
  await store.recordResult(asset.id, { version_num: 180006, monitor_role: true,
    connections: 90, max_connections: 100 });
  const [sample] = await store.listSamples(asset.id, 1);
  const result = evaluateInspection(changed, asset, sample, null);
  const id = randomUUID();
  await store.saveInspectionRun({ id, assetId: asset.id, templateId: changed.id,
    templateVersion: changed.version, sampleId: sample.id, retryOf: null,
    startedAt: new Date().toISOString(), completedAt: new Date().toISOString(), ...result });
  await store.upsertInspectionSchedule(asset.id, changed.id, 60);
  const due = new Date(Date.now() + 61 * 60000).toISOString();
  assert.equal((await store.claimDueInspections(due)).length, 1);
  assert.equal((await store.claimDueInspections(due)).length, 0);
  const now = Date.now();
  for (const [daysAgo, bytes] of [[2, 1000000], [1, 1200000]]) {
    const collectedAt = new Date(now - daysAgo * 86400000).toISOString();
    await store.ensureSampleMonth(collectedAt);
    await store.pool.query(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
      VALUES($1,$2,$3,'ok',$4)`, [randomUUID(), asset.id, collectedAt, JSON.stringify({ database_bytes: bytes })]);
  }
  await store.pool.query(`UPDATE samples SET metrics_json=$1 WHERE id=$2`,
    [JSON.stringify({ database_bytes: 1500000 }), sample.id]);
  const forecast = forecastDatabaseGrowth(await store.databaseCapacityPoints(asset.id));
  assert.equal(forecast.status, 'estimate');
  assert.equal(forecast.points.length, 3);
  assert.ok(forecast.projected30DayBytes > forecast.latestBytes);
  await store.close();
  store = await PgStore.create(process.env.DBSPHERE_DATABASE_URL);
  assert.equal((await store.getInspectionRun(id)).summary.risk, 1);
  assert.equal((await store.getInspectionTemplate('pg_daily')).version, changed.version);
  console.log('PG18 versioned inspection, history and schedule persistence passed');
} finally {
  if (asset) await store.deleteAsset(asset.id);
  await store.close();
}
