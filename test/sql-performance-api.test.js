import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { createApp } from '../src/server.js';
import { encrypt, loadMasterKey } from '../src/security.js';

test('authenticated Top SQL API returns reset-safe fingerprint deltas without SQL text', async t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const dataDir = mkdtempSync(join(root, 'dbsphere-sql-api-'));
  const oldPassword = process.env.DBSPHERE_ADMIN_PASSWORD;
  process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-sql-api-password';
  let calls = 0;
  const app = await createApp({ dataDir, collectFn: async () => {
    calls += 10;
    return { version_num: 180006, monitor_role: true, connections: 1, max_connections: 100,
      sqlPerformance: { status: 'ok', source: 'pg_stat_statements', resetAt: '2026-11-17T00:00:00Z',
        statements: [{ fingerprint: '1:2:3:1', queryId: '3', calls,
          totalExecMs: calls * 12, rows: 0, sharedBlocksHit: 0, sharedBlocksRead: 0,
          sharedBlocksWritten: 0, tempBlocksRead: 0, tempBlocksWritten: 0, walBytes: 0 }] } };
  } });
  t.after(async () => {
    await app.stop();
    if (oldPassword === undefined) delete process.env.DBSPHERE_ADMIN_PASSWORD;
    else process.env.DBSPHERE_ADMIN_PASSWORD = oldPassword;
    if (!resolve(dataDir).startsWith(root + sep)) throw new Error('Refusing to remove data outside test root');
    rmSync(dataDir, { recursive: true, force: true });
  });
  await new Promise(done => app.server.listen(0, '127.0.0.1', done));
  const base = `http://127.0.0.1:${app.server.address().port}`;
  const asset = app.store.addAsset({ name: 'SQL API target', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' },
  encrypt(loadMasterKey(dataDir), 'test-password'));
  assert.equal((await fetch(`${base}/api/assets/${asset.id}/sql`)).status, 401);
  const login = await fetch(`${base}/api/login`, { method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password: process.env.DBSPHERE_ADMIN_PASSWORD }) });
  const cookie = login.headers.get('set-cookie').split(';')[0];
  const { csrf } = await login.json();
  await app.collectAsset(asset.id);
  await app.collectAsset(asset.id);
  const response = await fetch(`${base}/api/assets/${asset.id}/sql`, { headers: { cookie } });
  assert.equal(response.status, 200);
  const { performance } = await response.json();
  assert.equal(performance.status, 'ok');
  assert.equal(performance.comparison, 'interval');
  assert.equal(performance.statements[0].delta.calls, 10);
  assert.equal(JSON.stringify(performance).includes('SELECT'), false);
  const log = '{"timestamp":"2026-11-17T08:00:00Z","dbname":"postgres",' +
    '"message":"duration: 212.3 ms  statement: SELECT secret_password",' +
    '"statement":"SELECT secret_password","query_id":3}';
  const importLog = () => fetch(`${base}/api/assets/${asset.id}/slow-logs/import`, {
    method: 'POST', headers: { cookie, 'content-type': 'application/json',
      'x-csrf-token': csrf }, body: JSON.stringify({ format: 'jsonlog', content: log })
  });
  assert.equal((await (await importLog()).json()).imported, 1);
  assert.equal((await (await importLog()).json()).duplicates, 1);
  const logs = await fetch(`${base}/api/assets/${asset.id}/slow-logs?from=2026-11-17T00:00:00Z&to=2026-11-18T00:00:00Z`,
    { headers: { cookie } });
  assert.equal(logs.status, 200);
  const text = await logs.text();
  assert.equal(JSON.parse(text).entries[0].queryId, '3');
  assert.equal(text.includes('secret_password'), false);
  const logId = JSON.parse(text).entries[0].id;
  const rawLog = await fetch(`${base}/api/assets/${asset.id}/slow-logs/${logId}`,
    { headers: { cookie } });
  assert.equal(rawLog.status, 200);
  assert.equal((await rawLog.json()).log.queryId, '3');
  assert.equal((await fetch(`${base}/api/assets/${asset.id}/slow-logs/${logId}`)).status, 401);
  const planBody = { queryId: '3', plan: [{ Plan: { 'Node Type': 'Seq Scan',
    'Total Cost': 100, Filter: "password = 'private'" }, 'Execution Time': 220 }] };
  assert.equal((await fetch(`${base}/api/assets/${asset.id}/sql-plans`, {
    method: 'POST', headers: { cookie, 'content-type': 'application/json' },
    body: JSON.stringify(planBody)
  })).status, 403);
  const savedPlan = await fetch(`${base}/api/assets/${asset.id}/sql-plans`, {
    method: 'POST', headers: { cookie, 'content-type': 'application/json',
      'x-csrf-token': csrf }, body: JSON.stringify(planBody)
  });
  assert.equal(savedPlan.status, 201);
  const planText = await (await fetch(`${base}/api/assets/${asset.id}/sql-plans`,
    { headers: { cookie } })).text();
  assert.equal(JSON.parse(planText).plans[0].summary.nodes[0].type, 'Seq Scan');
  assert.equal(planText.includes('private'), false);
  const planId = JSON.parse(planText).plans[0].id;
  const rawPlan = await fetch(`${base}/api/assets/${asset.id}/sql-plans/${planId}`,
    { headers: { cookie } });
  assert.equal(rawPlan.status, 200);
  assert.equal((await rawPlan.json()).plan.summary.nodes[0].type, 'Seq Scan');
});
