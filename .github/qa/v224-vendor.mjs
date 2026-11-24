import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { basename } from 'node:path';

const [engine, path] = process.argv.slice(2);
const root = 'http://127.0.0.1:18080';
const base = '/api/log-analysis/';
let cookie = '';
let csrf = '';

async function api(path, method = 'GET', data) {
  const binary = Buffer.isBuffer(data);
  const response = await fetch(root + path, {
    method, headers: {
      ...(cookie ? { Cookie: cookie } : {}), ...(csrf ? { 'X-CSRF-Token': csrf } : {}),
      ...(data ? { 'Content-Type': binary ? 'application/octet-stream' : 'application/json' } : {})
    },
    body: data ? binary ? data : JSON.stringify(data) : undefined
  });
  const text = await response.text();
  assert(response.ok, method + ' ' + path + ' HTTP ' + response.status + ' ' + text);
  if (!cookie && response.headers.get('Set-Cookie')) cookie = response.headers.get('Set-Cookie').split(';')[0];
  return JSON.parse(text);
}

const session = await api('/api/login', 'POST', { username: 'admin', password: process.env.DBSPHERE_ADMIN_PASSWORD });
csrf = session.csrf;
assert(csrf && cookie, 'Authentication did not return a session');
const organization = (await api('/api/admin/organizations')).organizations[0];
const asset = await api('/api/assets', 'POST', {
  name: engine + ' native log acceptance', organizationId: organization.id, host: '127.0.0.1', port: 15434,
  database: 'postgres', username: 'postgres', password: 'dbsphere-browser-fixture', runtime: 'managed', sslMode: 'disable'
});
const project = await api(base + 'projects', 'POST', { name: 'Real engine log acceptance', organizationId: organization.id });
const instance = await api(base + 'instances', 'POST', {
  name: engine, projectId: project.id, assetId: asset.id, environment: 'Acceptance', engine, version: '8'
});
const batch = await api(base + 'batches', 'POST', { instanceId: instance.id, name: 'Native engine trace', coverage: 'full' });
const bytes = await readFile(path);
const file = await api(base + 'batches/' + batch.id + '/files', 'POST', {
  name: basename(path), size: bytes.length, format: 'auto', zone: 'UTC'
});
for (let offset = 0, part = 0; offset < bytes.length; offset += file.chunkBytes, part++) {
  await api(base + 'files/' + file.id + '/blocks?part=' + part, 'PUT', bytes.subarray(offset, offset + file.chunkBytes));
}
await api(base + 'files/' + file.id + '/queue', 'POST');
let status;
for (let attempt = 0; attempt < 45; attempt++) {
  status = await api(base + 'files/' + file.id + '/status');
  if (['complete', 'partial', 'failed'].includes(status.state)) break;
  await new Promise(resolve => setTimeout(resolve, 500));
}
assert(['complete', 'partial'].includes(status.state), 'Native log failed to parse with auto format: ' + status.state);
const { records } = await api(base + 'batches/' + batch.id + '/records');
const matches = records.filter(row => row.sql?.toUpperCase().includes('DBS_V224_ACCEPT'));
const timed = matches.find(row => row.kind === 'statement' && row.durationMs != null);
if (!timed) {
  const { totals, batch: descriptor } = await api(base + 'batches/' + batch.id);
  throw new Error('No timed statement from native trace ' + JSON.stringify({ totals, format: descriptor.format,
    sample: bytes.toString('utf8').split(/\r?\n/).filter(line => line.includes('DBS_V224_ACCEPT')).slice(-4) }));
}
assert(!timed.sql.includes('dbsphere-dm8-fixture-literal'), 'Unmasked SQL literal in native report');
const report = await api(base + 'batches/' + batch.id + '/report');
assert(report.totals.timed > 0, 'No timed statements in the native report');
const source = await api(base + 'records/' + timed.id + '/original', 'POST');
assert(source.text.includes('DBS_V224_ACCEPT') && source.size > 0, 'Raw context does not match the native trace');
assert((await api(base + 'files/' + file.id + '/verify', 'POST')).verified, 'Archive checksum verification failed');
const investigation = await api(base + 'records/' + timed.id + '/investigate', 'POST', {});
assert(investigation.eventId && investigation.records.some(row => row.id === timed.id), 'Original record lost its event linkage');
assert((await api('/api/events/' + investigation.eventId)).event.id === investigation.eventId, 'Linked incident is unavailable');
const candidates = await api(base + 'events/' + investigation.eventId + '/candidates');
assert(candidates.eventId === investigation.eventId &&
  candidates.records.some(row => row.id === timed.id && row.caseId === investigation.id), 'Original evidence not listed on the same event');
console.log(JSON.stringify({ result: 'V224_NATIVE_LOG_PASS', engine, records: records.length, timed: report.totals.timed,
  event: investigation.eventId, record: timed.id, archiveVerified: true }));
