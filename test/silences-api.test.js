import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { createApp } from '../src/server.js';

test('silence API requires admin session and CSRF and records a bounded window', async t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const dataDir = mkdtempSync(join(root, 'dbsphere-silences-api-'));
  const previous = process.env.DBSPHERE_ADMIN_PASSWORD;
  process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-silence-test-password';
  const app = await createApp({ dataDir });
  t.after(async () => {
    await app.stop();
    if (previous === undefined) delete process.env.DBSPHERE_ADMIN_PASSWORD;
    else process.env.DBSPHERE_ADMIN_PASSWORD = previous;
    if (!resolve(dataDir).startsWith(root + sep)) throw new Error('Refusing to remove data outside the test root');
    rmSync(dataDir, { recursive: true, force: true });
  });
  await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
  const base = `http://127.0.0.1:${app.server.address().port}`;
  assert.equal((await fetch(`${base}/api/silences`)).status, 401);
  const login = await fetch(`${base}/api/login`, { method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password: process.env.DBSPHERE_ADMIN_PASSWORD }) });
  const cookie = login.headers.get('set-cookie').split(';')[0];
  const { csrf } = await login.json();
  const headers = { cookie, 'x-csrf-token': csrf, 'content-type': 'application/json' };
  const body = { kind: 'silence', reason: 'test window', endsAt: new Date(Date.now() + 3600 * 1000).toISOString() };
  assert.equal((await fetch(`${base}/api/silences`, { method: 'POST', headers: { cookie, 'content-type': 'application/json' },
    body: JSON.stringify(body) })).status, 403);
  assert.equal((await fetch(`${base}/api/silences`, { method: 'POST', headers,
    body: JSON.stringify({ ...body, assetId: 'not-an-id' }) })).status, 400);
  const created = await fetch(`${base}/api/silences`, { method: 'POST', headers, body: JSON.stringify(body) });
  assert.equal(created.status, 201);
  const { silence } = await created.json();
  assert.equal(silence.reason, 'test window');
  const listed = await (await fetch(`${base}/api/silences`, { headers: { cookie } })).json();
  assert.equal(listed.silences[0].id, silence.id);
  const revoked = await fetch(`${base}/api/silences/${silence.id}`, { method: 'DELETE', headers });
  assert.equal(revoked.status, 200);
  assert.ok((await revoked.json()).silence.revokedAt);
  assert.equal((await fetch(`${base}/api/silences/${silence.id}`, { method: 'DELETE', headers })).status, 404);
});
