#!/usr/bin/env node

// Verify PG18 kept the original five-version samples across Go and store Pod replacement.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

const [baseURL, credentialsPath, beforePath, afterPath] = process.argv.slice(2);
if (!baseURL || !credentialsPath || !beforePath || !afterPath) {
  console.error('usage: node scripts/kind-matrix-verify-persistence.mjs BASE_URL CREDENTIALS_PATH BEFORE_JSON AFTER_JSON');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const { adminPassword } = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const before = JSON.parse(readFileSync(beforePath, 'utf8'));
const after = JSON.parse(readFileSync(afterPath, 'utf8'));
assert.equal(before.storage, 'postgresql18');
assert.equal(after.storage, 'postgresql18');
assert.deepEqual(before.persistentClaims, after.persistentClaims);
assert.equal(before.matrix.length, 5);
assert.equal(after.matrix.length, 5);

const login = await fetch(base + '/api/login', {
  method: 'POST', headers: { 'content-type': 'application/json' },
  body: JSON.stringify({ password: adminPassword }), signal: AbortSignal.timeout(20000)
});
assert.equal(login.status, 200);
const cookie = login.headers.get('set-cookie')?.split(';')[0];
assert.ok(cookie);
const getSample = async (assetId, sampleId) => {
  const response = await fetch(`${base}/api/assets/${assetId}/samples/${sampleId}`, {
    headers: { Cookie: cookie }, signal: AbortSignal.timeout(20000)
  });
  assert.equal(response.status, 200, `sample ${sampleId} was lost after restart`);
  return (await response.json()).sample;
};
const preserved = [];
for (let major = 14; major <= 19; major++) {
  const old = before.matrix.find(item => item.major === major);
  const fresh = after.matrix.find(item => item.major === major);
  assert.ok(old && fresh, `PostgreSQL ${major} missing from matrix`);
  assert.equal(fresh.assetId, old.assetId, `PostgreSQL ${major} asset ID changed`);
  assert.notEqual(fresh.sampleId, old.sampleId, `PostgreSQL ${major} did not create a new sample`);
  for (const entry of [old, fresh]) {
    const sample = await getSample(entry.assetId, entry.sampleId);
    assert.equal(sample.status, 'ok');
    assert.equal(Math.floor(sample.metrics.version_num / 10000), major);
    assert.equal(sample.metrics.k8s?.status, 'ok');
  }
  preserved.push({ major, assetId: old.assetId, oldSampleId: old.sampleId,
    newSampleId: fresh.sampleId, node: fresh.node });
}
assert.ok(new Set(preserved.map(item => item.node)).size >= 2);
console.log(JSON.stringify({ storage: after.storage, preserved }));
