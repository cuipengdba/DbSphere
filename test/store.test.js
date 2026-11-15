import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { Store } from '../src/store.js';

test('incident evidence survives restart and rejects an unrelated OOM hypothesis', (t) => {
  const base = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(base, { recursive: true });
  const dir = mkdtempSync(join(base, 'dbsphere-store-'));
  t.after(() => { if (dir.startsWith(base + sep)) rmSync(dir, { recursive: true, force: true }); });
  let store = new Store(dir);
  const asset = store.addAsset({ name: 'pg', host: 'localhost', port: 5432, database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'k8s', k8sCluster: 'prod', namespace: 'db', pod: 'pg-0' }, 'encrypted-placeholder');
  store.recordResult(asset.id, { k8s: { status: 'ok', containers: [{ name: 'postgres', terminatedReason: 'OOMKilled', lastTerminatedAt: new Date().toISOString() }] } }, 'password authentication failed');
  const eventId = store.listEvents()[0].id;
  assert.equal(store.getEvent(eventId).evidence.filter(e => e.kind === 'root_cause_candidate').length, 0);
  store.close();
  store = new Store(dir);
  assert.equal(store.listSamples(asset.id).length, 1);
  assert.equal(store.getEvent(eventId).evidence.length, 2);
  store.close();
});
