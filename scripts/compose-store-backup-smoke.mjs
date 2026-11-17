import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const values = Object.fromEntries(readFileSync(new URL('../deploy/compose/.env', import.meta.url), 'utf8')
  .split(/\r?\n/).filter(line => line && !line.startsWith('#')).map(line => {
    const at = line.indexOf('=');
    return [line.slice(0, at), line.slice(at + 1)];
  }));
const base = `http://127.0.0.1:${values.DBSPHERE_WEB_PORT || 8080}`;
const login = await fetch(`${base}/api/login`, { method: 'POST',
  headers: { 'content-type': 'application/json' },
  body: JSON.stringify({ password: values.DBSPHERE_ADMIN_PASSWORD }) });
assert.equal(login.status, 200);
const cookie = login.headers.get('set-cookie').split(';')[0];
const response = await fetch(`${base}/api/storage`, { headers: { cookie } });
assert.equal(response.status, 200);
const { storage } = await response.json();
assert.equal(storage.backup.status, 'ok', storage.backup.reason);
assert.match(storage.backup.backupId, /^dbsphere-/);
assert.ok(storage.backup.restorePointAt);
console.log('Compose PG18 backup manifest is readable and visible in the authenticated B/S API');
