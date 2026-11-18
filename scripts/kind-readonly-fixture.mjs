#!/usr/bin/env node

// Replace a dedicated kind fault target's superuser monitoring credential with
// a PostgreSQL pg_monitor login. Never write generated credentials to the repo.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { existsSync, readFileSync, writeFileSync } from 'node:fs';

const [baseURL, adminPasswordPath, kubeconfigPath, namespace, eventId, monitorPasswordPath] = process.argv.slice(2);
if (!baseURL || !adminPasswordPath || !kubeconfigPath || !namespace || !eventId || !monitorPasswordPath) {
  console.error('usage: node scripts/kind-readonly-fixture.mjs BASE_URL ADMIN_PASSWORD_FILE KUBECONFIG NAMESPACE EVENT_ID MONITOR_PASSWORD_FILE');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const monitorPassword = existsSync(monitorPasswordPath)
  ? readFileSync(monitorPasswordPath, 'utf8').trim()
  : randomBytes(32).toString('base64url');
assert.match(monitorPassword, /^[A-Za-z0-9_-]{32,}$/);
const login = await fetch(base + '/api/login', { method: 'POST',
  headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ password: readFileSync(adminPasswordPath, 'utf8').trim() }),
  signal: AbortSignal.timeout(10000) });
assert.equal(login.status, 200);
const cookie = login.headers.get('set-cookie')?.split(';')[0];
const csrf = (await login.json()).csrf;
assert.ok(cookie && csrf);
const headers = { Cookie: cookie, 'X-CSRF-Token': csrf, 'Content-Type': 'application/json' };
const eventResponse = await fetch(`${base}/api/events/${eventId}`, { headers,
  signal: AbortSignal.timeout(10000) });
assert.equal(eventResponse.status, 200);
const { event } = await eventResponse.json();
const assetResponse = await fetch(`${base}/api/assets/${event.assetId}`, { headers,
  signal: AbortSignal.timeout(10000) });
assert.equal(assetResponse.status, 200);
const { asset } = await assetResponse.json();
assert.equal(asset.runtime, 'k8s');
assert.equal(asset.namespace, namespace);
assert.equal(asset.workload, 'dbsphere-go-oom-pg');
assert.equal(asset.host, 'dbsphere-go-oom-pg');

const role = 'dbsphere_acceptance_monitor';
const sql = `DO $fixture$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='${role}') THEN
    CREATE ROLE ${role} LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '${monitorPassword}';
  ELSE
    ALTER ROLE ${role} LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '${monitorPassword}';
  END IF;
END $fixture$;
GRANT pg_monitor TO ${role};
`;
execFileSync('kubectl', ['--kubeconfig', kubeconfigPath, '-n', namespace,
  'exec', '-i', 'deploy/dbsphere-go-oom-pg', '--', 'psql', '-U', 'postgres',
  '-v', 'ON_ERROR_STOP=1'], { input: sql, encoding: 'utf8', timeout: 30000 });
if (!existsSync(monitorPasswordPath)) writeFileSync(monitorPasswordPath, `${monitorPassword}\n`, { mode: 0o600 });
const update = await fetch(`${base}/api/assets/${asset.id}`, { method: 'PATCH', headers,
  body: JSON.stringify({
    name: asset.name, host: asset.host, port: asset.port, database: asset.database,
    username: role, password: monitorPassword, sslMode: asset.sslMode,
    runtime: asset.runtime, k8sCluster: asset.k8sCluster,
    namespace: asset.namespace, workload: asset.workload, pod: asset.pod || '',
    node: asset.node || ''
  }), signal: AbortSignal.timeout(10000) });
assert.equal(update.status, 200, `asset update failed: ${update.status}`);
const checked = await fetch(`${base}/api/assets/${asset.id}/check`,
  { method: 'POST', headers, signal: AbortSignal.timeout(45000) });
assert.equal(checked.status, 200);
const { result } = await checked.json();
assert.equal(result.status, 'ok');
const sampleResponse = await fetch(`${base}/api/assets/${asset.id}/samples/${result.sampleId}`,
  { headers, signal: AbortSignal.timeout(10000) });
assert.equal(sampleResponse.status, 200);
const { sample } = await sampleResponse.json();
assert.equal(sample.metrics.monitor_role, true);
assert.equal(sample.metrics.privileged_role, false);
console.log(JSON.stringify({ assetId: asset.id, sampleId: result.sampleId,
  role, monitorRole: true, privilegedRole: false }));
