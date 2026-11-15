import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const envPath = new URL('../deploy/compose/.env', import.meta.url);
const composePath = new URL('../deploy/compose/compose.yaml', import.meta.url);
const values = Object.fromEntries(readFileSync(envPath, 'utf8').trim().split(/\r?\n/)
  .filter(line => line && !line.startsWith('#')).map(line => {
    const at = line.indexOf('=');
    return [line.slice(0, at), line.slice(at + 1)];
  }));
const base = `http://127.0.0.1:${values.DBSPHERE_WEB_PORT || 8080}`;
const docker = (...args) => execFileSync('docker', ['compose', '--env-file', fileURLToPath(envPath),
  '-f', fileURLToPath(composePath), ...args], { stdio: 'ignore' });
async function waitStatus(path, expected, attempts = 90) {
  for (let i = 0; i < attempts; i++) {
    try {
      const response = await fetch(base + path, { signal: AbortSignal.timeout(2000) });
      if (response.status === expected) return response;
    } catch { /* Waiting for service transition. */ }
    await new Promise(resolve => setTimeout(resolve, 500));
  }
  throw new Error(`${path} did not return ${expected}`);
}
async function login() {
  const response = await fetch(`${base}/api/login`, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password: values.DBSPHERE_ADMIN_PASSWORD })
  });
  assert.equal(response.status, 200);
  return { cookie: response.headers.get('set-cookie').split(';')[0],
    csrf: (await response.json()).csrf };
}

const ready = await waitStatus('/readyz', 200);
assert.equal((await ready.json()).storage, 'postgresql18');
let auth = await login();
const headers = { cookie: auth.cookie, 'x-csrf-token': auth.csrf,
  'content-type': 'application/json' };
const created = await fetch(`${base}/api/assets`, { method: 'POST', headers,
  body: JSON.stringify({ name: 'Compose smoke', host: '127.0.0.1', port: 1,
    database: 'postgres', username: 'monitor', password: 'test-only-secret',
    sslMode: 'disable', runtime: 'linux', linuxHost: 'localhost' }) });
assert.equal(created.status, 201);
const assetId = (await created.json()).asset.id;
const checked = await fetch(`${base}/api/assets/${assetId}/check`, { method: 'POST', headers });
assert.equal(checked.status, 200);
assert.equal((await checked.json()).result.status, 'error');
docker('restart', 'app');
await waitStatus('/readyz', 200);
auth = await login();
const assets = await (await fetch(`${base}/api/assets`, { headers: { cookie: auth.cookie } })).json();
assert.ok(assets.assets.some(asset => asset.id === assetId && asset.lastStatus === 'error'));
const samples = await (await fetch(`${base}/api/assets/${assetId}/samples`, {
  headers: { cookie: auth.cookie }
})).json();
assert.equal(samples.samples.length, 1);
const events = await (await fetch(`${base}/api/events`, { headers: { cookie: auth.cookie } })).json();
assert.equal(events.events.length, 1);

docker('stop', 'store');
try { await waitStatus('/readyz', 503); }
finally { docker('start', 'store'); }
await waitStatus('/readyz', 200);
console.log('One-click Compose: PG18 readiness, persistent asset/sample/event and store outage passed');
