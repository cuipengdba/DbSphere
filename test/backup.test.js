import test from 'node:test';
import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { collectBackup, parsePgBackRestInfo } from '../src/backup.js';
import { DEFAULT_INSPECTION_TEMPLATES, evaluateInspection } from '../src/inspections.js';

test('pgBackRest adapter distinguishes missing, stale, failed and healthy backup signals', async t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const directory = mkdtempSync(join(root, 'dbsphere-backup-'));
  t.after(() => {
    if (!resolve(directory).startsWith(root + sep)) throw new Error('Refusing to remove data outside test root');
    rmSync(directory, { recursive: true, force: true });
  });
  const asset = { id: randomUUID(), runtime: 'linux' };
  const template = DEFAULT_INSPECTION_TEMPLATES.find(item => item.id === 'backup_daily');
  const now = Date.now();
  const absent = await collectBackup(asset, directory);
  assert.equal(absent.status, 'unavailable');
  const unknown = evaluateInspection(template, asset, { status: 'ok', collectedAt: new Date(now).toISOString(),
    metrics: { backup: absent } }, null);
  assert.equal(unknown.summary.unknown, 3);
  const raw = [{ name: 'main', status: { code: 0, message: 'ok' },
    backup: [{ timestamp: { stop: Math.floor((now - 30 * 3600000) / 1000) } }] }];
  writeFileSync(join(directory, `${asset.id}.json`), JSON.stringify(raw));
  const stale = await collectBackup(asset, directory);
  assert.equal(stale.repositoryHealthy, true);
  const checked = evaluateInspection(template, asset, { status: 'ok', collectedAt: new Date(now).toISOString(),
    metrics: { backup: stale } }, null);
  assert.equal(checked.findings.find(item => item.id === 'backup_age').status, 'risk');
  assert.equal(checked.findings.find(item => item.id === 'repository').status, 'pass');
  assert.equal(checked.findings.find(item => item.id === 'last_attempt').status, 'unknown');
  const wrapper = { pgbackrest: [{ ...raw[0], status: { code: 28, message: 'stanza unavailable' } }],
    lastAttempt: { status: 'failed', at: new Date(now - 3600000).toISOString() } };
  writeFileSync(join(directory, `${asset.id}.json`), JSON.stringify(wrapper));
  const failed = await collectBackup(asset, directory);
  const failures = evaluateInspection(template, asset, { status: 'ok', collectedAt: new Date(now).toISOString(),
    metrics: { backup: failed } }, null);
  assert.equal(failures.summary.risk, 3);
  assert.equal(parsePgBackRestInfo([{ name: 'main', status: {}, backup: [] }]).status, 'unavailable');
});
