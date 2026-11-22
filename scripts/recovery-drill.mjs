#!/usr/bin/env node

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { createHash } from 'node:crypto';
import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { setTimeout as delay } from 'node:timers/promises';

const [base, restoredBase, credentialsPath, envPath, reportsPath, resultPath] = process.argv.slice(2);
if (!base || !restoredBase || !credentialsPath || !envPath || !reportsPath || !resultPath) {
  throw new Error('Usage: recovery-drill.mjs BASE RESTORED_BASE CREDENTIALS ENV_FILE REPORT_DIR RESULT');
}
const secret = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const reportDir = resolve(reportsPath);
mkdirSync(reportDir, {recursive: true});
const compose = ['compose', '--env-file', envPath, '-f', 'test/recovery-drill/compose.yaml'];
const backrestOptions = ['--config=/etc/dbsphere-pgbackrest.conf', '--stanza=ledger'];

function docker(args, input, binary = false) {
  try {
    return execFileSync('docker', [...compose, ...args], {input, encoding: binary ? undefined : 'utf8',
      maxBuffer: 40 * 1024 * 1024, timeout: 140000, stdio: ['pipe', 'pipe', 'pipe']});
  } catch (err) {
    let message = [err.stderr, binary ? '' : err.stdout].filter(Boolean).map(String).join('\n') || err.message;
    for (const value of Object.values(secret)) message = message.replaceAll(value, '[redacted]');
    throw new Error('Isolated recovery command failed: ' + message.slice(-4000));
  }
}
const business = (...args) => docker(['exec', '-T', '-u', 'postgres', 'business', ...args]);
const backrest = (...args) => business('pgbackrest', ...backrestOptions, ...args);
function sql(service, query, extra = []) {
  const user = service === 'business' ? 'postgres' : 'dbsphere';
  return docker(['exec', '-T', service, 'psql', '-U', user, '-d', user, '-v', 'ON_ERROR_STOP=1', '-Atq', ...extra],
    query + '\n').trim();
}
const clock = service => sql(service, "SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"');");
function publish(name, body) {
  const file = join(reportDir, name);
  writeFileSync(file + '.tmp', JSON.stringify(body) + '\n', {mode: 0o644});
  renameSync(file + '.tmp', file);
}
async function waitUntil(label, check, timeout = 120000) {
  const end = Date.now() + timeout;
  do {
    const value = await check();
    if (value) return value;
    await delay(1500);
  } while (Date.now() < end);
  throw new Error('Timed out waiting for ' + label);
}
function client(url) {
  let cookie = '';
  let csrf = '';
  return async (path, method = 'GET', data) => {
    const response = await fetch(url + path, {method, headers: {
      ...(cookie ? {Cookie: cookie} : {}), ...(csrf && method !== 'GET' ? {'X-CSRF-Token': csrf} : {}),
      ...(data ? {'Content-Type': 'application/json'} : {})
    }, body: data ? JSON.stringify(data) : undefined, signal: AbortSignal.timeout(45000)});
    const body = await response.json();
    assert.ok(response.ok, method + ' ' + path + ': ' + response.status + ' ' + JSON.stringify(body));
    if (path === '/api/login') {
      cookie = response.headers.get('set-cookie')?.split(';')[0] || '';
      csrf = body.csrf;
      assert.ok(cookie && csrf);
    }
    return body;
  };
}
const api = client(base);
const restoredAPI = client(restoredBase);
assert.equal((await api('/readyz')).storage, 'postgresql18');
await api('/api/login', 'POST', {password: secret.adminPassword});
await api('/api/alert-rules/pg_archive_failures', 'PATCH', {severity: 'critical', durationSeconds: 0});

sql('business', "CREATE ROLE dbsphere_monitor LOGIN PASSWORD '" + secret.monitorPassword + "';\n" +
  'GRANT pg_monitor TO dbsphere_monitor;\nCREATE TABLE recovery_marker (id text PRIMARY KEY);\n' +
  "INSERT INTO recovery_marker VALUES ('base-backup');");
backrest('stanza-create');
backrest('--type=full', 'backup');
const asset = (await api('/api/assets', 'POST', {name: '业务库 PITR 实测', clusterName: '备份恢复演练',
  host: 'business', port: 5432, database: 'postgres', username: 'dbsphere_monitor', password: secret.monitorPassword,
  sslMode: 'disable', runtime: 'linux'})).asset;
const info = () => JSON.parse(backrest('--output=json', 'info'));
const initialCatalog = info();
const full = initialCatalog[0].backup.find(row => row.type === 'full' && !row.error);
assert.ok(full?.label);
sql('business', "INSERT INTO recovery_marker VALUES ('diff-backup');");
backrest('--type=diff', 'backup');
const catalog = info();
const diff = catalog[0].backup.find(row => row.type === 'diff' && !row.error);
assert.ok(diff?.prior === full.label);
const segmentBytes = Number(sql('business', "SELECT pg_size_bytes(current_setting('wal_segment_size'));"));
const archive = catalog[0].archive.find(row => row.database.id === diff.database.id && row.database['repo-key'] === 1);
assert.ok(archive?.min && archive.max);
const earliest = diff.archive.start;
const segments = () => Object.keys(JSON.parse(backrest('--output=json', '--recurse', 'repo-ls', 'archive/ledger')))
  .map(path => path.split('/').at(-1).match(/^([0-9A-F]{24})-/)?.[1]).filter(Boolean);
const report = (catalog, last, tests = []) => ({
  pgbackrest: catalog, lastAttempt: {status: 'success', at: new Date(diff.timestamp.stop * 1000).toISOString()},
  walChecks: [{repoKey: 1, databaseId: diff.database.id, from: earliest, to: last, segmentBytes, segments: segments()}],
  restoreTests: tests, targetRpoSeconds: 60, targetRtoSeconds: 180
});
async function collect() {
  const check = await api('/api/assets/' + asset.id + '/check', 'POST');
  assert.equal(check.result.status, 'ok');
  return (await api('/api/assets/' + asset.id + '/samples/' + check.result.sampleId)).sample;
}

publish(asset.id + '.json', report(catalog, archive.max));
const baseline = await collect();
assert.ok(baseline.metrics.backup.jobs.some(row => row.label === diff.label &&
  row.dependenciesAvailable && row.chain.join(',') === [full.label, diff.label].join(',')));
const marker = name => sql('business', "INSERT INTO recovery_marker VALUES ('" + name + "');\n" +
  "SELECT to_char(pg_xact_commit_timestamp(xmin) AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') " +
  "FROM recovery_marker WHERE id='" + name + "';");
let archiveFault, targetAt, beforeAt, afterAt, blockedWal;
business('touch', '/lab/archive-paused');
try {
  beforeAt = marker('before-target');
  targetAt = clock('business');
  await delay(100);
  afterAt = marker('after-target');
  blockedWal = sql('business', 'SELECT pg_walfile_name(pg_current_wal_insert_lsn());');
  sql('business', 'SELECT pg_switch_wal();');
  await waitUntil('actual PostgreSQL archive failure for the pending WAL', () =>
    sql('business', 'SELECT last_failed_wal FROM pg_stat_archiver;') === blockedWal);
  publish(asset.id + '.json', report(info(), blockedWal));
  archiveFault = await collect();
  const gap = archiveFault.metrics.backup.walChecks[0];
  assert.ok(gap.missing.includes(blockedWal) && gap.missingCount >= 1);
} finally {
  business('unlink', '/lab/archive-paused');
}

const findEvent = async () => (await api('/api/events')).events.find(row =>
  row.assetId === asset.id && row.eventGroup === 'storage');
const incident = await findEvent();
assert.equal(incident.status, 'open');
const opened = await api('/api/events/' + incident.id);
const candidate = opened.analysis.candidates.find(row => row.code === 'wal_archive_failures');
assert.ok(candidate?.supports.includes('sample:' + archiveFault.id));
console.log(JSON.stringify({phase: 'archive-gap', eventId: incident.id, wal: blockedWal, sampleId: archiveFault.id}));
await waitUntil('the initially missing segment to reach the repository', () => segments().includes(blockedWal));

const businessStarted = clock('business');
const postgresTarget = targetAt.replace('T', ' ').replace(/Z$/, '+00');
backrest('--pg1-path=/lab/restored', '--set=' + diff.label, '--type=time', '--target=' + postgresTarget,
  '--target-action=pause', '--target-timeline=current', 'restore');
let restoreStarted = false;
try {
  business('pg_ctl', '-D', '/lab/restored', '-l', '/lab/restored-postgres.log',
    '-o', '-p 5442 -k /lab/restore-socket -c listen_addresses=127.0.0.1 -c archive_mode=off', '-w', 'start');
  restoreStarted = true;
  const lookup = ['-h', '/lab/restore-socket', '-p', '5442'];
  await waitUntil('the physically restored database to pause at the requested target', () =>
    sql('business', 'SELECT pg_is_in_recovery() AND pg_get_wal_replay_pause_state()=\'paused\';', lookup) === 't');
  const rows = sql('business', 'SELECT id FROM recovery_marker ORDER BY id;', lookup).split(/\r?\n/);
  assert.deepEqual(rows, ['base-backup', 'before-target', 'diff-backup']);
} catch (err) {
  try { console.error(business('tail', '-n', '24', '/lab/restored-postgres.log')); } catch { /* retain the original failure */ }
  throw err;
} finally {
  if (restoreStarted) business('pg_ctl', '-D', '/lab/restored', '-m', 'fast', '-w', 'stop');
}
const businessProof = {id: 'business-pitr-verified', repoKey: 1, databaseId: diff.database.id, backup: diff.label,
  targetAt, startedAt: businessStarted, finishedAt: clock('business'), sourceCommitAt: afterAt,
  restoredCommitAt: beforeAt, afterCommitAt: afterAt, isolated: true, beforeMarker: 'before-target', afterMarker: 'after-target',
  confirmedBefore: true, excludedAfter: true};
publish(asset.id + '.json', report(info(), blockedWal, [businessProof]));
let businessRecovery = await collect();
if ((await findEvent()).status === 'open') businessRecovery = await collect();
assert.equal(businessRecovery.metrics.backup.walChecks[0].missingCount, 0);
const checkedBusiness = businessRecovery.metrics.backup.restoreTests[0];
assert.equal(checkedBusiness.verification, 'verified');
assert.ok(checkedBusiness.rtoSeconds > 0 && checkedBusiness.rpoSeconds > 0);
assert.equal((await findEvent()).id, incident.id);
assert.equal((await findEvent()).status, 'resolved');

sql('store', 'CREATE TABLE recovery_marker (id text PRIMARY KEY);\n' +
  "INSERT INTO recovery_marker (id) VALUES ('before-dump');");
const beforeStore = sql('store', "SELECT to_char(pg_xact_commit_timestamp(xmin) AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') " +
  "FROM recovery_marker WHERE id='before-dump';");
const restorePointAt = clock('store');
const dump = docker(['exec', '-T', 'store', 'pg_dump', '-U', 'dbsphere', '-d', 'dbsphere', '-Fc'], undefined, true);
assert.ok(dump.length > 4096);
const archiveSha256 = createHash('sha256').update(dump).digest('hex');
const dumpPath = join(dirname(resolve(resultPath)), 'monitor-store-test.dump');
writeFileSync(dumpPath, dump, {mode: 0o600});
assert.equal(createHash('sha256').update(readFileSync(dumpPath)).digest('hex'), archiveSha256);
const completedAt = clock('store');
await delay(40);
const afterStore = sql('store', "INSERT INTO recovery_marker (id) VALUES ('after-dump');\n" +
  "SELECT to_char(pg_xact_commit_timestamp(xmin) AT TIME ZONE 'UTC','YYYY-MM-DD\"T\"HH24:MI:SS.US\"Z\"') FROM recovery_marker WHERE id='after-dump';");
const storeStarted = clock('store');
docker(['exec', '-T', 'store-restore', 'pg_restore', '-U', 'dbsphere', '-d', 'dbsphere', '--no-owner',
  '--no-acl', '--exit-on-error'], readFileSync(dumpPath));
docker(['up', '-d', '--wait', '--wait-timeout', '100', 'restored-console']);
await waitUntil('the restored Go console to report PostgreSQL 18 storage', async () =>
  (await restoredAPI('/readyz').catch(() => ({}))).storage === 'postgresql18');
await restoredAPI('/api/login', 'POST', {password: secret.adminPassword});
const restoredEvent = await restoredAPI('/api/events/' + incident.id);
const restoredAsset = (await restoredAPI('/api/assets')).assets.find(row => row.id === asset.id);
const restoredRule = (await restoredAPI('/api/alert-rules')).rules.find(row => row.id === 'pg_archive_failures');
const restoredSample = (await restoredAPI('/api/assets/' + asset.id + '/samples/' + archiveFault.id)).sample;
const eventsReadable = restoredEvent.event.id === incident.id &&
  restoredEvent.event.evidence.some(row => row.payload?.sampleId === archiveFault.id);
const configurationReadable = restoredAsset?.host === asset.host && restoredRule?.severity === 'critical';
const samplesReadable = restoredSample.id === archiveFault.id &&
  restoredSample.metrics.backup.walChecks[0].missing.includes(blockedWal);
assert.ok(eventsReadable && configurationReadable && samplesReadable);
assert.equal(sql('store-restore', 'SELECT string_agg(id,\',\') FROM recovery_marker;'), 'before-dump');
const backupId = 'dbsphere-go-restore-drill';
const storeProof = {id: 'pg18-store-verified', backupId, archiveSha256, isolated: true, targetAt: restorePointAt,
  startedAt: storeStarted, finishedAt: clock('store'), sourceCommitAt: afterStore, restoredCommitAt: beforeStore,
  afterCommitAt: afterStore, beforeMarker: 'before-dump', afterMarker: 'after-dump', confirmedBefore: true,
  excludedAfter: true, eventsReadable, configurationReadable, samplesReadable};
const storeManifest = {status: 'success', backupId, sha256: archiveSha256, completedAt, restorePointAt};
publish('monitor-store.json', {...storeManifest, restoreTests: [{...storeProof, archiveSha256: '0'.repeat(64)}]});
assert.equal((await api('/api/storage')).storage.backup.restoreTests.length, 0);
publish('monitor-store.json', {...storeManifest, restoreTests: [storeProof]});
const recordedStore = await api('/api/storage');
assert.equal(recordedStore.storage.backup.restoreTests[0].verification, 'verified');
assert.ok(recordedStore.storage.backup.restoreTests[0].rpoSeconds > 0);
const query = new URLSearchParams({from: new Date(Date.parse(baseline.collectedAt) - 1000).toISOString(),
  to: new Date(Date.now() + 1000).toISOString(), sample: archiveFault.id});
const history = await api('/api/assets/' + asset.id + '/backup?' + query);
assert.ok(history.observations.some(row => row.id === archiveFault.id && row.report.walChecks[0].missing.includes(blockedWal)));
assert.ok(history.observations.some(row => row.id === businessRecovery.id && row.report.restoreTests[0].verification === 'verified'));
const result = {eventId: incident.id, assetId: asset.id, faultSampleId: archiveFault.id, recoverySampleId: businessRecovery.id,
  blockedWal, full: full.label, differential: diff.label, business: checkedBusiness, store: recordedStore.storage.backup.restoreTests[0]};
const artifact = JSON.stringify(result, null, 2);
for (const value of Object.values(secret)) assert.ok(!artifact.includes(value), 'A credential escaped into recovery evidence');
writeFileSync(resultPath, artifact);
console.log(artifact);
