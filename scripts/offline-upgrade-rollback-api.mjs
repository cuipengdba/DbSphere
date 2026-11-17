import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { join } from 'node:path';

const mode = process.argv[2];
const assetId = process.argv[3];
if (!['create', 'check'].includes(mode) || (mode === 'check' && !assetId)) {
  throw new Error('Usage: node scripts/offline-upgrade-rollback-api.mjs create|check [asset-id]');
}
const env = await readFile(join(process.env.DBSPHERE_CONFIG_DIR, 'dbsphere.env'), 'utf8');
const password = env.match(/^DBSPHERE_ADMIN_PASSWORD=(.*)$/m)?.[1];
assert.ok(password, 'Missing generated administrator credential');
const base = 'http://127.0.0.1:8080';
const login = await fetch(`${base}/api/login`, { method: 'POST',
  headers: { 'content-type': 'application/json' }, body: JSON.stringify({ password }) });
assert.equal(login.status, 200);
const csrf = (await login.json()).csrf;
const cookie = login.headers.get('set-cookie')?.split(';')[0];
assert.ok(cookie);
if (mode === 'create') {
  const created = await fetch(`${base}/api/assets`, { method: 'POST',
    headers: { 'content-type': 'application/json', 'x-csrf-token': csrf, cookie },
    body: JSON.stringify({ name: 'Offline upgrade persistence', host: '127.0.0.1', port: 5432,
      database: 'postgres', username: 'monitor', password: 'not-a-real-target-secret',
      sslMode: 'disable', runtime: 'linux' }) });
  const result = await created.json();
  assert.equal(created.status, 201, result.error);
  process.stdout.write(result.asset.id);
} else {
  const assets = await fetch(`${base}/api/assets`, { headers: { cookie } });
  assert.equal(assets.status, 200);
  assert.ok((await assets.json()).assets.some(asset => asset.id === assetId));
}
