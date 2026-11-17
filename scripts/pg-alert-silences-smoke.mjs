import assert from 'node:assert/strict';
import { PgStore } from '../src/pg-store.js';
import { normalizeSilence } from '../src/silences.js';

if (!process.env.DBSPHERE_DATABASE_URL) throw new Error('Set DBSPHERE_DATABASE_URL');
const store = await PgStore.create(process.env.DBSPHERE_DATABASE_URL);
try {
  const asset = await store.addAsset({ name: 'PG18 silence smoke', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted-test-placeholder');
  const silence = await store.createSilence(normalizeSilence({ kind: 'maintenance', assetId: asset.id,
    ruleId: 'pg_collection_failed', reason: 'PG18 maintenance smoke',
    endsAt: new Date(Date.now() + 3600000).toISOString() }));
  assert.ok(silence?.id);
  await store.recordResult(asset.id, {}, 'password authentication failed');
  const alert = (await store.listActiveAlerts()).find(item => item.assetId === asset.id && item.ruleId === 'pg_collection_failed');
  assert.equal(alert.state.mutedBy, silence.id);
  const event = await store.getEvent(alert.state.eventId);
  assert.equal(event.status, 'open');
  assert.equal(event.evidence.filter(item => item.kind === 'alert_suppressed').length, 1);
  assert.equal(event.evidence.filter(item => item.kind === 'collection_error').length, 1);
  assert.ok((await store.listSilences()).some(item => item.id === silence.id));
  await store.revokeSilence(silence.id);
  await store.recordResult(asset.id, {}, 'password authentication failed');
  const unmuted = (await store.listActiveAlerts()).find(item => item.assetId === asset.id && item.ruleId === 'pg_collection_failed');
  assert.equal(unmuted.state.mutedBy, null);
  assert.equal((await store.getEvent(event.id)).evidence.filter(item => item.kind === 'alert_unsuppressed').length, 1);
  await store.recordResult(asset.id, { version_num: 180006, connections: 1, max_connections: 100 });
  assert.equal((await store.getEvent(event.id)).status, 'resolved');
  console.log('PostgreSQL 18 silence keeps incident evidence and re-enables alerts after revoke');
} finally { await store.close(); }
