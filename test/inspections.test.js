import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { Store } from '../src/store.js';
import { evaluateInspection } from '../src/inspections.js';

test('versioned inspections persist changes, report unknown data and claim schedules once', t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const directory = mkdtempSync(join(root, 'dbsphere-inspections-'));
  const store = new Store(directory);
  t.after(() => {
    store.close();
    if (!resolve(directory).startsWith(root + sep)) throw new Error('Refusing to remove data outside test root');
    rmSync(directory, { recursive: true, force: true });
  });
  const asset = store.addAsset({ name: 'PG14', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted-placeholder');
  const original = store.getInspectionTemplate('pg_daily');
  assert.equal(original.version, 1);
  assert.equal(store.updateInspectionTemplate('pg_daily', { checks: [] }), null);
  const changed = store.updateInspectionTemplate('pg_daily', { thresholds: { connections: 80 } });
  assert.equal(changed.version, 2);
  assert.equal(changed.checks.find(item => item.id === 'connections').threshold, 80);
  store.recordResult(asset.id, { version_num: 140024, monitor_role: false,
    connections: 90, max_connections: 100 });
  const [sample] = store.listSamples(asset.id, 1);
  const first = evaluateInspection(changed, asset, sample, null);
  assert.ok(first.findings.some(item => item.id === 'pg14_eol' && item.status === 'risk'));
  assert.ok(first.findings.some(item => item.id === 'connections' && item.status === 'risk'));
  assert.ok(first.findings.some(item => item.id === 'locks' && item.status === 'unknown'));
  const run = { id: randomUUID(), assetId: asset.id, templateId: changed.id,
    templateVersion: changed.version, sampleId: sample.id, retryOf: null,
    startedAt: new Date().toISOString(), completedAt: new Date().toISOString(), ...first };
  store.saveInspectionRun(run);
  store.recordResult(asset.id, { version_num: 180006, monitor_role: true,
    connections: 10, max_connections: 100 });
  const [nextSample, priorSample] = store.listSamples(asset.id, 2);
  const next = evaluateInspection(changed, asset, nextSample, priorSample, store.getInspectionRun(run.id));
  assert.ok(next.summary.changed >= 3);
  assert.equal(next.findings.find(item => item.id === 'pg14_eol').previousStatus, 'risk');
  const schedule = store.upsertInspectionSchedule(asset.id, changed.id, 60);
  assert.ok(schedule);
  const due = new Date(Date.now() + 61 * 60000).toISOString();
  assert.equal(store.claimDueInspections(due).length, 1);
  assert.equal(store.claimDueInspections(due).length, 0);
  assert.equal(store.listInspectionRuns(asset.id)[0].templateVersion, 2);
});
