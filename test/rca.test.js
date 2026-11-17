import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { analyzeEvent, eventSampleIds } from '../src/rca.js';
import { createApp } from '../src/server.js';

const eventId = '11111111-1111-4111-8111-111111111111';
const sampleId = '22222222-2222-4222-8222-222222222222';
const evidenceId = '33333333-3333-4333-8333-333333333333';
const base = { id: eventId, assetId: '44444444-4444-4444-8444-444444444444',
  assetName: 'pg18', title: '锁等待', startedAt: '2026-11-17T00:00:00Z', evidence: [] };

test('lock evidence stays under one incident and exposes blocking pair', () => {
  const event = { ...base, evidence: [{ id: evidenceId, kind: 'alert_firing',
    observedAt: '2026-11-17T00:01:00Z', payload: { metric: 'pg.blocked_sessions', sampleId } }] };
  const sample = { id: sampleId, collectedAt: '2026-11-17T00:01:00Z', status: 'ok',
    metrics: { diagnostics: { lockChain: { status: 'ok', data: [
      { waitingPid: 21, blockerPid: 10, waitingSince: '2026-11-17T00:00:30Z' }
    ] } }, k8s: { status: 'ok', pod: { uid: 'pod-1', name: 'pg-0' },
      node: { name: 'worker-1' }, pvcs: [{ name: 'pg-data' }] } } };
  assert.deepEqual(eventSampleIds(event), [sampleId]);
  const analysis = analyzeEvent(event, [sample]);
  assert.equal(analysis.eventId, eventId);
  assert.equal(analysis.status, 'candidates');
  assert.equal(analysis.candidates[0].code, 'lock_chain');
  assert.deepEqual(analysis.candidates[0].detail.pairs[0],
    { waitingPid: 21, blockerPid: 10, waitingSince: '2026-11-17T00:00:30Z' });
  assert.ok(analysis.graph.edges.some(edge => edge.from === `evidence:${evidenceId}` &&
    edge.to === `sample:${sampleId}`));
  assert.ok(analysis.graph.edges.some(edge => edge.from === 'candidate:lock_chain' &&
    edge.to === `sample:${sampleId}`));
  assert.ok(analysis.graph.nodes.some(node => node.id === 'pod:pod-1'));
});

test('alert without diagnostic proof remains unlocated', () => {
  const event = { ...base, evidence: [{ id: evidenceId, kind: 'alert_firing',
    observedAt: '2026-11-17T00:01:00Z', payload: { metric: 'pg.blocked_sessions', sampleId } }] };
  const analysis = analyzeEvent(event, [{ id: sampleId, status: 'ok', metrics: {
    diagnostics: { lockChain: { status: 'unavailable', reason: 'permission denied' } }
  } }]);
  assert.equal(analysis.status, 'unlocated');
  assert.equal(analysis.candidates.length, 0);
  assert.ok(analysis.missingSignals.some(signal => signal.includes('锁链')));
});

test('administrator confirms a supported candidate with CSRF and a recorded reason', async t => {
  const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || tmpdir());
  mkdirSync(root, { recursive: true });
  const directory = mkdtempSync(join(root, 'dbsphere-rca-'));
  const priorPassword = process.env.DBSPHERE_ADMIN_PASSWORD;
  process.env.DBSPHERE_ADMIN_PASSWORD = 'rca-test-admin-password';
  const app = await createApp({ dataDir: directory,
    collectFn: async () => { throw new Error('connection refused'); },
    k8sFn: async () => ({ status: 'ok', pod: { uid: 'pod-1', name: 'pg-0' },
      containers: [{ name: 'postgres', terminatedReason: 'OOMKilled',
        lastTerminatedAt: new Date().toISOString() }], pvcs: [] }) });
  t.after(async () => {
    await app.stop();
    if (priorPassword === undefined) delete process.env.DBSPHERE_ADMIN_PASSWORD;
    else process.env.DBSPHERE_ADMIN_PASSWORD = priorPassword;
    if (!resolve(directory).startsWith(root + sep)) throw new Error('Unexpected test directory');
    rmSync(directory, { recursive: true, force: true });
  });
  await new Promise(resolve => app.server.listen(0, '127.0.0.1', resolve));
  const baseUrl = `http://127.0.0.1:${app.server.address().port}`;
  const login = await fetch(`${baseUrl}/api/login`, { method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password: process.env.DBSPHERE_ADMIN_PASSWORD }) });
  const csrf = (await login.json()).csrf;
  const cookie = login.headers.get('set-cookie').split(';')[0];
  const headers = { 'content-type': 'application/json', cookie, 'x-csrf-token': csrf };
  const created = await fetch(`${baseUrl}/api/assets`, { method: 'POST', headers,
    body: JSON.stringify({ name: 'rca test', host: '127.0.0.1', port: 5432,
      database: 'postgres', username: 'monitor', password: 'dummy', sslMode: 'disable',
      runtime: 'k8s', k8sCluster: 'test', namespace: 'database', pod: 'pg-0' }) });
  const asset = (await created.json()).asset;
  assert.equal(created.status, 201);
  await fetch(`${baseUrl}/api/assets/${asset.id}/check`, { method: 'POST', headers });
  const event = (await (await fetch(`${baseUrl}/api/events`, { headers: { cookie } })).json()).events[0];
  const path = `${baseUrl}/api/events/${event.id}/rca-decision`;
  assert.equal((await fetch(path, { method: 'POST', headers: { cookie, 'content-type': 'application/json' },
    body: JSON.stringify({ code: 'pod_oom_correlated', decision: 'confirmed', reason: '同一 Pod 的 OOM 时间先于连接失败' }) })).status, 403);
  assert.equal((await fetch(path, { method: 'POST', headers,
    body: JSON.stringify({ code: 'made_up', decision: 'confirmed', reason: '没有任何支持证据' }) })).status, 400);
  assert.equal((await fetch(path, { method: 'POST', headers,
    body: JSON.stringify({ code: 'pod_oom_correlated', decision: 'confirmed',
      reason: '同一 Pod 的 OOM 时间先于连接失败' }) })).status, 200);
  const detail = await (await fetch(`${baseUrl}/api/events/${event.id}`, { headers: { cookie } })).json();
  assert.equal(detail.analysis.eventId, event.id);
  assert.equal(detail.analysis.candidates[0].review.decision, 'confirmed');
  assert.equal(detail.event.evidence.filter(item => item.kind === 'rca_decision').length, 1);
});
