#!/usr/bin/env node

// Read-only post-restart verification of the previously completed incident.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const [baseURL, credentialsPath, resultPath] = process.argv.slice(2);
if (!baseURL || !credentialsPath || !resultPath) {
  console.error('usage: node scripts/replication-drill-verify.mjs BASE_URL CREDENTIALS_PATH RESULT_PATH');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const { adminPassword } = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const { eventId, primaryId, faultSampleId, recoverySampleId } = JSON.parse(readFileSync(resultPath, 'utf8'));
const login = await fetch(base + '/api/login', { method: 'POST',
  headers: { 'content-type': 'application/json' }, body: JSON.stringify({ password: adminPassword }),
  signal: AbortSignal.timeout(10000) });
assert.equal(login.status, 200);
const cookie = login.headers.get('set-cookie')?.split(';')[0];
assert.ok(cookie);
const get = async path => {
  const response = await fetch(base + path, { headers: { Cookie: cookie }, signal: AbortSignal.timeout(20000) });
  assert.equal(response.status, 200, path);
  return response.json();
};
const ready = await (await fetch(base + '/readyz', { signal: AbortSignal.timeout(10000) })).json();
assert.equal(ready.storage, 'postgresql18');
const fault = (await get(`/api/assets/${primaryId}/samples/${faultSampleId}`)).sample;
const recovery = (await get(`/api/assets/${primaryId}/samples/${recoverySampleId}`)).sample;
assert.ok(Number.isFinite(fault.metrics.replication_lag_bytes) &&
  fault.metrics.replication_lag_bytes >= 64 * 1024 * 1024);
assert.equal(recovery.metrics.replication_observed, true);
assert.equal(recovery.metrics.replicas, 1);
assert.ok(Number.isFinite(recovery.metrics.replication_lag_bytes) &&
  recovery.metrics.replication_lag_bytes < 64 * 1024 * 1024);
const detail = await get(`/api/events/${eventId}`);
assert.equal(detail.event.id, eventId);
assert.equal(detail.event.status, 'resolved');
assert.ok(detail.analysis.candidates.some(item => item.code === 'replication_lag'));
assert.ok(detail.event.evidence.some(item => item.kind === 'alert_firing'));
assert.ok(detail.event.evidence.some(item => item.kind === 'alert_recovered' &&
  item.payload?.sampleId === recoverySampleId));
assert.ok(detail.analysis.graph.nodes.some(item => item.id === `sample:${faultSampleId}`));
assert.ok(detail.analysis.graph.nodes.some(item => item.id === `sample:${recoverySampleId}`));
console.log(JSON.stringify({ eventId, status: detail.event.status,
  storage: ready.storage, faultSampleId, faultLagBytes: fault.metrics.replication_lag_bytes,
  recoverySampleId, recoveryLagBytes: recovery.metrics.replication_lag_bytes,
  replicasAtRecovery: recovery.metrics.replicas }));
