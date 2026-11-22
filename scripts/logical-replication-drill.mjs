#!/usr/bin/env node

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';

const [base, credentialsPath, envPath, resultPath, pgMajor = '18'] = process.argv.slice(2);
if (!base || !credentialsPath || !envPath || !resultPath) {
  throw new Error('Usage: logical-replication-drill.mjs BASE CREDENTIALS ENV_FILE RESULT [PG_MAJOR]');
}
const secret = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const compose = ['compose', '--env-file', envPath, '-f', 'test/logical-replication/compose.yaml'];
const slotName = 'dbsphere_ledger_slot';
const subscriptionName = 'dbsphere_ledger_sub';
const publicationName = 'dbsphere_ledger_pub';
const threshold = 6 * 1024 * 1024;
let cookie = '';
let csrf = '';

function psql(service, sql) {
  try {
    return execFileSync('docker', [...compose, 'exec', '-T', service, 'psql', '-U', 'postgres',
      '-d', 'postgres', '-v', 'ON_ERROR_STOP=1', '-Atq'], {input: sql + '\n', encoding: 'utf8', timeout: 120000,
      stdio: ['pipe', 'pipe', 'pipe']}).trim();
  } catch (err) {
    let message = String(err.stderr || err.message);
    for (const value of Object.values(secret)) message = message.replaceAll(value, '[redacted]');
    throw new Error('PostgreSQL ' + service + ' fixture failed: ' + message);
  }
}

async function api(path, method = 'GET', payload) {
  const response = await fetch(base + path, {
    method,
    headers: { ...(cookie ? {Cookie: cookie} : {}), ...(csrf && method !== 'GET' ? {'X-CSRF-Token': csrf} : {}),
      ...(payload ? {'Content-Type': 'application/json'} : {}) },
    body: payload ? JSON.stringify(payload) : undefined,
    signal: AbortSignal.timeout(45000)
  });
  const body = await response.json();
  assert.ok(response.ok, method + ' ' + path + ' → ' + response.status + ' ' + JSON.stringify(body));
  return {body, response};
}

async function waitUntil(label, predicate, timeout = 120000) {
  const deadline = Date.now() + timeout;
  do {
    const value = await predicate();
    if (value) return value;
    await delay(2500);
  } while (Date.now() < deadline);
  throw new Error('Timed out waiting for ' + label);
}

async function asset(host, username = 'dbsphere_monitor') {
  const name = username === 'dbsphere_monitor' ? '逻辑复制 ' + host : '只读权限受限';
  const created = await api('/api/assets', 'POST', {name, clusterName: '逻辑复制演练', host, port: 5432,
    database: 'postgres', username, password: secret.monitorPassword, sslMode: 'disable', runtime: 'linux'});
  return created.body.asset;
}

async function collect(asset) {
  const check = await api('/api/assets/' + asset.id + '/check', 'POST');
  assert.equal(check.body.result.status, 'ok');
  const record = await api('/api/assets/' + asset.id + '/samples/' + check.body.result.sampleId);
  assert.equal(Math.floor(record.body.sample.metrics.version_num / 10000), Number(pgMajor));
  return record.body.sample;
}

const diag = (sample, key) => sample.metrics.diagnostics[key];
const slot = sample => diag(sample, 'slotDetails').data.find(row => row.name === slotName);
const subscription = sample => diag(sample, 'logicalSubscriptions').data.entries.find(row => row.name === subscriptionName);

assert.equal((await api('/readyz')).body.storage, 'postgresql18');
const monitorRole = "CREATE ROLE dbsphere_monitor LOGIN PASSWORD '" + secret.monitorPassword + "';\n" +
  'GRANT pg_monitor TO dbsphere_monitor;';
const table = 'CREATE TABLE public.ledger (id int PRIMARY KEY,payload text NOT NULL);';
psql('publisher', monitorRole + '\n' + table +
  "\nCREATE ROLE dbsphere_feed LOGIN REPLICATION PASSWORD '" + secret.replPassword + "';\n" +
  'GRANT SELECT ON public.ledger TO dbsphere_feed;\nCREATE PUBLICATION ' + publicationName + ' FOR TABLE public.ledger;');
psql('subscriber', monitorRole + '\n' + table +
  '\nGRANT SELECT ON pg_catalog.pg_replication_origin_status TO dbsphere_monitor;' +
  '\nGRANT EXECUTE ON FUNCTION pg_catalog.pg_show_replication_origin_status() TO dbsphere_monitor;' +
  "\nCREATE SUBSCRIPTION " + subscriptionName + " CONNECTION 'host=publisher port=5432 dbname=postgres " +
  'user=dbsphere_feed sslmode=disable password=' + secret.replPassword + "' PUBLICATION " + publicationName +
  " WITH (copy_data=false,slot_name='" + slotName + "');");
psql('publisher', "INSERT INTO public.ledger VALUES (1,'baseline');");
await waitUntil('the first publisher row to reach the subscriber',
  () => psql('subscriber', 'SELECT count(*) FROM public.ledger WHERE id=1;') === '1');

const login = await api('/api/login', 'POST', {password: secret.adminPassword});
cookie = login.response.headers.get('set-cookie')?.split(';')[0] || '';
csrf = login.body.csrf;
assert.ok(cookie && csrf);
const publisher = await asset('publisher');
const subscriber = await asset('subscriber');
await api('/api/alert-rules/pg_logical_slot_retention', 'PATCH', {threshold, durationSeconds: 0});
const baseline = await collect(publisher);
const originalSubscriber = await collect(subscriber);
assert.equal(slot(baseline).active, true);
assert.equal(slot(baseline).type, 'logical');
assert.equal(slot(baseline).plugin, 'pgoutput');
assert.ok(diag(baseline, 'logicalPublications').data.entries.some(row => row.name === publicationName && row.explicitTables === 1));
assert.equal(subscription(originalSubscriber).state, 'running');
assert.equal(subscription(originalSubscriber).slot, slotName);
assert.ok(subscription(originalSubscriber).publications.includes(publicationName));
assert.equal(subscription(originalSubscriber).readyTableCount, 1);
assert.equal(originalSubscriber.metrics.pgCompatibility.signals.find(row => row.key === 'logicalSubscriptionErrors').status,
  Number(pgMajor) === 14 ? 'not_supported' : 'ok');
console.log(JSON.stringify({phase: 'baseline', publisherSample: baseline.id, subscriberSample: originalSubscriber.id}));

psql('subscriber', 'ALTER SUBSCRIPTION ' + subscriptionName + ' DISABLE;');
await waitUntil('the publisher slot to become inactive', () =>
  psql('publisher', "SELECT NOT active FROM pg_replication_slots WHERE slot_name='" + slotName + "';") === 't');
const stoppedSubscriber = await collect(subscriber);
assert.equal(subscription(stoppedSubscriber).state, 'disabled');
assert.equal(subscription(stoppedSubscriber).applyPid, null);
psql('publisher', "INSERT INTO public.ledger SELECT n, (SELECT string_agg(md5(n::text||':'||k::text),'') " +
  'FROM generate_series(1,64) k) FROM generate_series(2,5500) n;');
const growth = await collect(publisher);
assert.equal(slot(growth).active, false);
assert.ok(Number(slot(growth).unconfirmedBytes) > threshold);
assert.ok(Number(slot(growth).retainedBytes) > Number(slot(baseline).retainedBytes) + threshold);
assert.equal(psql('subscriber', 'SELECT count(*) FROM public.ledger;'), '1');

const incidents = () => api('/api/events').then(({body}) =>
  body.events.filter(row => row.assetId === publisher.id && row.eventGroup === 'replication'));
const firing = await incidents();
assert.equal(firing.length, 1);
const eventId = firing[0].id;
assert.equal(firing[0].status, 'open');
console.log(JSON.stringify({phase: 'disabled', eventId, sampleId: growth.id, slotName, bytes: slot(growth).retainedBytes}));

psql('subscriber', 'ALTER SUBSCRIPTION ' + subscriptionName + ' ENABLE;');
await waitUntil('all 5500 rows to reach the subscriber', () =>
  psql('subscriber', 'SELECT count(*) FROM public.ledger;') === '5500', 180000);
const caughtUp = await collect(subscriber);
assert.equal(subscription(caughtUp).state, 'running');
const applyProgress = diag(caughtUp, 'logicalApplyProgress');
assert.equal(applyProgress.status, 'ok');
assert.ok(applyProgress.data.some(row => row.oid === subscription(caughtUp).oid && row.appliedOriginLsn && row.appliedOriginLsn !== '0/0'));
if (Number(pgMajor) >= 15) assert.ok(diag(caughtUp, 'logicalSubscriptionErrors').data.some(row =>
  row.oid === subscription(caughtUp).oid && Number.isFinite(row.syncErrors)));
const recovered = await waitUntil('slot restart position and retained WAL to advance', async () => {
  psql('publisher', "INSERT INTO public.ledger VALUES (6000,'feedback') ON CONFLICT (id) DO UPDATE SET payload='feedback';\nCHECKPOINT;");
  const current = await collect(publisher);
  const entry = slot(current);
  return entry?.active && entry.restartLsn !== slot(growth).restartLsn && Number(entry.retainedBytes) < threshold
    ? current : null;
}, 180000);
const finalEvents = await incidents();
assert.equal(finalEvents.length, 1);
assert.equal(finalEvents[0].id, eventId);
assert.equal(finalEvents[0].status, 'resolved');
const detail = (await api('/api/events/' + eventId)).body;
const candidate = detail.analysis.candidates.find(row => row.code === 'logical_slot_wal_retention');
assert.ok(candidate?.supports.includes('sample:' + growth.id));
assert.ok(detail.event.evidence.some(item => item.kind === 'alert_recovered' && item.payload?.sampleId === recovered.id));
const query = new URLSearchParams({from: new Date(Date.parse(baseline.collectedAt) - 1000).toISOString(),
  to: new Date(Date.now() + 1000).toISOString(), sample: growth.id});
const publisherHistory = (await api('/api/assets/' + publisher.id + '/replication?' + query)).body;
const subscriberHistory = (await api('/api/assets/' + subscriber.id + '/replication?' + query)).body;
assert.ok(Number(publisherHistory.observations.find(row => row.kind === 'slot_peak').logicalSlotPressure.data.retainedBytes) >=
  Number(slot(growth).retainedBytes));
assert.equal(publisherHistory.observations.find(row => row.kind === 'incident').id, growth.id);
assert.ok(!subscriberHistory.observations.some(row => row.kind === 'incident'));
assert.ok(subscriberHistory.observations.some(row => row.logicalSubscriptions?.data.entries.some(entry =>
  entry.name === subscriptionName && entry.state === 'running')));

psql('subscriber', "CREATE ROLE dbsphere_limited LOGIN PASSWORD '" + secret.monitorPassword + "';\n" +
  'REVOKE SELECT ON pg_catalog.pg_stat_subscription FROM PUBLIC;\n' +
  'GRANT SELECT ON pg_catalog.pg_stat_subscription TO dbsphere_monitor;');
const restricted = await asset('subscriber', 'dbsphere_limited');
const limited = await collect(restricted);
assert.equal(diag(limited, 'logicalSubscriptions').status, 'unavailable');
assert.equal(diag(limited, 'logicalSubscriptions').reason, 'SQLSTATE 42501');
assert.equal(diag(limited, 'logicalPublications').status, 'ok');
assert.equal(diag(limited, 'slotDetails').status, 'ok');

const proof = {publisher, subscriber, restricted, detail, baseline, originalSubscriber, stoppedSubscriber, growth, caughtUp,
  recovered, limited, publisherHistory, subscriberHistory};
const encoded = JSON.stringify(proof);
for (const value of Object.values(secret)) assert.ok(!encoded.includes(value), 'credentials leaked into replay evidence');
writeFileSync(resultPath.replace(/\.json$/, '.browser.json'), encoded);
const result = {eventId, targetPGMajor: Number(pgMajor), publisherId: publisher.id, subscriberId: subscriber.id,
  restrictedId: restricted.id, slotName, subscriptionName, publicationName, baselineSampleId: baseline.id,
  disabledSampleId: stoppedSubscriber.id, growthSampleId: growth.id, recoveredSampleId: recovered.id,
  caughtUpSampleId: caughtUp.id, retainedBefore: Number(slot(baseline).retainedBytes),
  retainedDuring: Number(slot(growth).retainedBytes), retainedAfter: Number(slot(recovered).retainedBytes),
  applyProgress: applyProgress.status, limitedSubscriptionStatus: diag(limited, 'logicalSubscriptions').reason,
  candidate: candidate.code, status: detail.event.status};
writeFileSync(resultPath, JSON.stringify(result, null, 2));
console.log(JSON.stringify(result, null, 2));
