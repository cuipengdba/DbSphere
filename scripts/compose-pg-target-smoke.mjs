import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const envText = readFileSync(new URL('../deploy/compose/.env', import.meta.url), 'utf8');
const values = Object.fromEntries(envText.trim().split(/\r?\n/).map(line => line.split(/=(.*)/s).slice(0, 2)));
const base = `http://127.0.0.1:${values.DBSPHERE_WEB_PORT || 8080}`;
const login = await fetch(`${base}/api/login`, { method: 'POST', headers: { 'content-type': 'application/json' },
  body: JSON.stringify({ password: values.DBSPHERE_ADMIN_PASSWORD }) });
assert.equal(login.status, 200);
const cookie = login.headers.get('set-cookie').split(';')[0];
const csrf = (await login.json()).csrf;
const headers = { cookie, 'x-csrf-token': csrf, 'content-type': 'application/json' };
const created = await fetch(`${base}/api/assets`, { method: 'POST', headers,
  body: JSON.stringify({ name: 'Compose PG14 target', host: 'host.docker.internal', port: 15435,
    database: 'postgres', username: 'dbsphere_monitor', password: 'dbsphere-monitor-test',
    sslMode: 'disable', runtime: 'linux' }) });
assert.equal(created.status, 201);
const id = (await created.json()).asset.id;
const checked = await fetch(`${base}/api/assets/${id}/check`, { method: 'POST', headers });
assert.equal(checked.status, 200);
assert.equal((await checked.json()).result.status, 'ok');
const sample = (await (await fetch(`${base}/api/assets/${id}/samples`, { headers: { cookie } })).json()).samples[0];
assert.equal(Math.floor(sample.metrics.version_num / 10000), 14);
assert.equal(sample.metrics.pg14_eol, true);
console.log('One-click Compose: real PostgreSQL 14 target was onboarded and sampled');
