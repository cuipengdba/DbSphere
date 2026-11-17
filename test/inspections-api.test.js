import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { createApp } from '../src/server.js';
import { encrypt, loadMasterKey } from '../src/security.js';

test('manual inspection, schedule and retry require login and CSRF and keep a history', async t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const dataDir = mkdtempSync(join(root, 'dbsphere-inspection-api-'));
  const oldPassword = process.env.DBSPHERE_ADMIN_PASSWORD;
  process.env.DBSPHERE_ADMIN_PASSWORD = 'dbsphere-inspection-api-password';
  let holdCollection = false;
  let releaseCollection;
  const app = await createApp({ dataDir, collectFn: async () => {
    if (holdCollection) await new Promise(done => { releaseCollection = done; });
    return { version_num: 180006, monitor_role: true, connections: 5, max_connections: 100 };
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
  const asset = app.store.addAsset({ name: 'Inspection API target', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' },
  encrypt(loadMasterKey(dataDir), 'test-password'));
  assert.equal((await fetch(`${base}/api/inspection-templates`)).status, 401);
  const login = await fetch(`${base}/api/login`, { method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password: process.env.DBSPHERE_ADMIN_PASSWORD }) });
  const cookie = login.headers.get('set-cookie').split(';')[0];
  const { csrf } = await login.json();
  const post = (path, method, payload, token = csrf) => fetch(`${base}${path}`, { method,
    headers: { cookie, 'content-type': 'application/json', 'x-csrf-token': token },
    body: JSON.stringify(payload) });
  const path = '/api/inspections/run';
  const payload = { assetId: asset.id, templateId: 'pg_daily' };
  assert.equal((await post(path, 'POST', payload, 'wrong')).status, 403);
  const firstResponse = await post(path, 'POST', payload);
  assert.equal(firstResponse.status, 201);
  const first = (await firstResponse.json()).run;
  assert.equal(first.assetId, asset.id);
  assert.ok(first.findings.some(item => item.id === 'locks' && item.status === 'unknown'));
  assert.equal((await fetch(`${base}/api/inspection-runs/${first.id}/report`)).status, 401);
  const markdown = await fetch(`${base}/api/inspection-runs/${first.id}/report`, { headers: { cookie } });
  assert.equal(markdown.status, 200);
  assert.match(markdown.headers.get('content-type'), /text\/markdown/);
  assert.match(await markdown.text(), new RegExp(first.sampleId));
  const html = await fetch(`${base}/api/inspection-runs/${first.id}/report?format=html`, { headers: { cookie } });
  assert.equal(html.status, 200);
  assert.match(await html.text(), /<!doctype html>/);
  const schedule = await post('/api/inspection-schedules', 'PUT', { ...payload,
    intervalMinutes: 1440, enabled: true });
  assert.equal(schedule.status, 200);
  assert.equal((await post('/api/inspection-schedules', 'PUT', { ...payload,
    intervalMinutes: 1, enabled: true })).status, 400);
  const retryResponse = await post(`/api/inspection-runs/${first.id}/retry`, 'POST');
  assert.equal(retryResponse.status, 201);
  const retry = (await retryResponse.json()).run;
  assert.equal(retry.retryOf, first.id);
  const history = await (await fetch(`${base}/api/inspection-runs?assetId=${asset.id}`, { headers: { cookie } })).json();
  assert.equal(history.runs.length, 2);
  assert.equal(history.runs[0].id, retry.id);
  holdCollection = true;
  const active = app.collectAsset(asset.id);
  const failed = (await (await post(path, 'POST', payload)).json()).run;
  assert.equal(failed.status, 'failed');
  assert.equal(failed.sampleId, null);
  assert.match(failed.error, /采集正忙/);
  releaseCollection();
  await active;
  holdCollection = false;
  const recovered = (await (await post(`/api/inspection-runs/${failed.id}/retry`, 'POST')).json()).run;
  assert.equal(recovered.status, 'completed');
  assert.equal(recovered.retryOf, failed.id);
});
