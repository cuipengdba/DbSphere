import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const envText = readFileSync(new URL('../deploy/compose/.env', import.meta.url), 'utf8');
const values = Object.fromEntries(envText.trim().split(/\r?\n/).map(line => line.split(/=(.*)/s).slice(0, 2)));
const base = `http://127.0.0.1:${values.DBSPHERE_WEB_PORT || 8080}`;
const ready = await fetch(`${base}/readyz`);
assert.equal(ready.status, 200);
assert.equal((await ready.json()).storage, 'postgresql18');
const login = await fetch(`${base}/api/login`, { method: 'POST', headers: { 'content-type': 'application/json' },
  body: JSON.stringify({ password: values.DBSPHERE_ADMIN_PASSWORD }) });
assert.equal(login.status, 200);
const cookie = login.headers.get('set-cookie').split(';')[0];
const assets = (await (await fetch(`${base}/api/assets`, { headers: { cookie } })).json()).assets;
const pg14 = assets.find(asset => asset.name === 'Compose PG14 target');
const error = assets.find(asset => asset.name === 'Compose smoke');
assert.equal(pg14?.lastStatus, 'ok');
assert.equal(error?.lastStatus, 'error');
assert.equal((await (await fetch(`${base}/api/assets/${pg14.id}/samples`, { headers: { cookie } })).json()).samples.length, 1);
const events = (await (await fetch(`${base}/api/events`, { headers: { cookie } })).json()).events;
assert.ok(events.some(event => event.assetId === error.id && event.status === 'open'));
console.log('One-click Compose rebuild: PG14 target, PG18 samples and error event persisted');
