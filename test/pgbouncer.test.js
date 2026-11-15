import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { createApp } from '../src/server.js';
import { collectPgBouncer } from '../src/pgbouncer.js';

test('PgBouncer URL stays encrypted and its status joins the PostgreSQL failure event', async () => {
  const base = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  const dir = await mkdtemp(join(base, 'dbsphere-pooler-'));
  process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-pooler-test-password';
  const poolerUrl = 'postgres://stats_user:pooler-secret@pooler.internal:6432/pgbouncer';
  const app = await createApp({ dataDir: dir,
    collectFn: async () => { throw new Error('connection refused'); },
    linuxFn: async () => ({ status: 'not_configured' }),
    poolerFn: async (_asset, secret) => {
      assert.equal(secret, poolerUrl);
      return { status: 'ok', pools: [{ clientWaiting: 4 }], stats: [] };
    } });
  await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
  const baseUrl = `http://127.0.0.1:${app.server.address().port}`;
  try {
    const login = await fetch(`${baseUrl}/api/login`, { method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ password: process.env.DBSPHERE_ADMIN_PASSWORD }) });
    const cookie = login.headers.get('set-cookie').split(';')[0];
    const csrf = (await login.json()).csrf;
    const headers = { cookie, 'x-csrf-token': csrf, 'content-type': 'application/json' };
    const input = { name: 'PG with pooler', host: '127.0.0.1', port: 5432,
      database: 'postgres', username: 'monitor', password: 'pg-secret', sslMode: 'disable',
      runtime: 'linux', poolerUrl };
    const created = await fetch(`${baseUrl}/api/assets`, { method: 'POST', headers, body: JSON.stringify(input) });
    assert.equal(created.status, 201);
    const asset = (await created.json()).asset;
    assert.equal(asset.poolerConfigured, true);
    assert.equal(JSON.stringify(asset).includes('pooler-secret'), false);
    assert.equal((await app.collectAsset(asset.id)).status, 'error');
    const event = (await app.store.listEvents())[0];
    const detail = await app.store.getEvent(event.id);
    assert.equal(detail.evidence.find(e => e.kind === 'pgbouncer_state').payload.pools[0].clientWaiting, 4);
    const changed = await fetch(`${baseUrl}/api/assets/${asset.id}`, { method: 'PATCH', headers,
      body: JSON.stringify({ ...input, password: '', poolerUrl: '' }) });
    assert.equal((await changed.json()).asset.poolerConfigured, true);
    const cleared = await fetch(`${baseUrl}/api/assets/${asset.id}`, { method: 'PATCH', headers,
      body: JSON.stringify({ ...input, password: '', poolerUrl: '', poolerClear: true }) });
    assert.equal((await cleared.json()).asset.poolerConfigured, false);
    assert.equal((await collectPgBouncer({ database_name: 'postgres' }, '')).status, 'not_configured');
  } finally {
    await app.stop();
    if (!resolve(dir).startsWith(base + sep)) throw new Error('Refusing to remove outside the test root');
    await rm(dir, { recursive: true, force: true });
  }
});
