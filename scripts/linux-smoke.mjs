import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { createServer } from 'node:net';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

if (process.platform !== 'linux') throw new Error('This smoke test must run on Linux');

const dataDir = await mkdtemp(join(tmpdir(), 'dbsphere-linux-smoke-'));
const password = 'dbsphere-linux-smoke-strong-password';
const listener = createServer();
await new Promise(resolve => listener.listen(0, '127.0.0.1', resolve));
const port = listener.address().port;
await new Promise(resolve => listener.close(resolve));
const base = `http://127.0.0.1:${port}`;

let child;
let output = '';
async function start() {
  output = '';
  child = spawn(process.execPath, ['src/server.js'], {
    cwd: new URL('..', import.meta.url),
    env: { ...process.env, DBSPHERE_DATA_DIR: dataDir, DBSPHERE_ADMIN_PASSWORD: password,
      DBSPHERE_HOST: '127.0.0.1', DBSPHERE_PORT: String(port) },
    stdio: ['ignore', 'pipe', 'pipe']
  });
  for (const stream of [child.stdout, child.stderr]) stream.on('data', chunk => {
    output = (output + chunk.toString()).slice(-5000);
  });
  for (let i = 0; i < 300; i++) {
    if (child.exitCode !== null) throw new Error(`DbSphere exited: ${child.exitCode}\n${output}`);
    try {
      const response = await fetch(`${base}/healthz`);
      if (response.ok) return;
    } catch { /* Waiting for the Linux process to listen. */ }
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error(`Linux server did not become healthy (pid ${child.pid}, exit ${child.exitCode})\n${output}`);
}

async function stop() {
  if (!child || child.exitCode !== null) return;
  const done = new Promise(resolve => child.once('exit', resolve));
  child.kill('SIGTERM');
  await done;
  child = undefined;
}

async function login() {
  const response = await fetch(`${base}/api/login`, {
    method: 'POST', headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password })
  });
  assert.equal(response.status, 200);
  return { cookie: response.headers.get('set-cookie').split(';')[0], csrf: (await response.json()).csrf };
}

try {
  await start();
  let auth = await login();
  const assetResponse = await fetch(`${base}/api/assets`, {
    method: 'POST', headers: { 'content-type': 'application/json', cookie: auth.cookie,
      'x-csrf-token': auth.csrf },
    body: JSON.stringify({ name: 'Linux smoke', host: '127.0.0.1', port: 1,
      database: 'postgres', username: 'monitor', password: 'test-secret',
      sslMode: 'disable', runtime: 'linux', linuxHost: 'localhost' })
  });
  assert.equal(assetResponse.status, 201);
  const asset = (await assetResponse.json()).asset;
  const check = await fetch(`${base}/api/assets/${asset.id}/check`, {
    method: 'POST', headers: { cookie: auth.cookie, 'x-csrf-token': auth.csrf }
  });
  assert.equal(check.status, 200);
  assert.equal((await check.json()).result.status, 'error');
  await stop();

  await start();
  auth = await login();
  const assets = await (await fetch(`${base}/api/assets`, { headers: { cookie: auth.cookie } })).json();
  assert.equal(assets.assets.length, 1);
  assert.equal(assets.assets[0].id, asset.id);
  assert.equal(assets.assets[0].lastStatus, 'error');
  const samples = await (await fetch(`${base}/api/assets/${asset.id}/samples`, {
    headers: { cookie: auth.cookie }
  })).json();
  assert.equal(samples.samples.length, 1);
  const events = await (await fetch(`${base}/api/events`, { headers: { cookie: auth.cookie } })).json();
  assert.equal(events.events.length, 1);
  assert.equal(events.events[0].status, 'open');
  console.log('Linux native process: health, persistent asset/sample/event, disconnected PG state passed');
} finally {
  await stop();
  await rm(dataDir, { recursive: true, force: true });
}
