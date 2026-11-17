import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { readStoreBackupManifest } from '../src/store-backup.js';

test('PG18 store backup manifest reports age and restore point without assuming missing backup is healthy', async t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const directory = mkdtempSync(join(root, 'dbsphere-store-backup-'));
  t.after(() => {
    if (!resolve(directory).startsWith(root + sep)) throw new Error('Refusing to remove data outside test root');
    rmSync(directory, { recursive: true, force: true });
  });
  const path = join(directory, 'manifest.json');
  const now = Date.parse('2026-11-17T08:00:00Z');
  assert.equal((await readStoreBackupManifest(null, now)).status, 'not_configured');
  assert.equal((await readStoreBackupManifest(path, now)).status, 'unavailable');
  writeFileSync(path, JSON.stringify({ status: 'success', backupId: 'backup-001',
    completedAt: '2026-11-17T06:00:00Z', restorePointAt: '2026-11-17T05:59:00Z' }));
  const valid = await readStoreBackupManifest(path, now);
  assert.equal(valid.status, 'ok');
  assert.equal(valid.ageHours, 2);
  assert.equal(valid.restorePointAt, '2026-11-17T05:59:00.000Z');
  writeFileSync(path, JSON.stringify({ status: 'success', backupId: 'backup-001',
    completedAt: '2026-11-17T06:00:00Z', restorePointAt: '2026-11-17T07:00:00Z' }));
  assert.equal((await readStoreBackupManifest(path, now)).status, 'unavailable');
});
