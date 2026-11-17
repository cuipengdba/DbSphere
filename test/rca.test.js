import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, rmSync } from 'node:fs';
import { join, resolve, sep } from 'node:path';
import { tmpdir } from 'node:os';
import { analyzeEvent, eventSampleIds } from '../src/rca.js';
import { createApp } from '../src/server.js';
import { oomCandidate } from '../src/incidents.js';

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

test('a future OOM timestamp cannot explain an earlier connection failure', () => {
  const result = { k8s: { containers: [{ name: 'postgres', terminatedReason: 'OOMKilled',
    lastTerminatedAt: new Date(Date.now() + 60_000).toISOString() }] } };
  assert.equal(oomCandidate(result, 'connection refused', evidenceId, sampleId), null);
});

test('OOM candidate reports its observed ordering and linked conditions', () => {
  const event = { ...base, startedAt: '2026-11-17T00:01:00Z', evidence: [{ id: evidenceId,
    kind: 'root_cause_candidate', observedAt: '2026-11-17T00:01:00Z', payload: {
      hypothesis: 'Pod OOM may cause PG connection failure', confidence: 'medium',
      supports: [], causeObservedAt: '2026-11-17T00:00:00Z', limitation: 'Temporal correlation only'
    } }] };
  const candidate = analyzeEvent(event, []).candidates[0];
  assert.equal(candidate.timeOrder.relation, 'before_event');
  assert.equal(candidate.matchedConditions[0].metric, 'k8s.OOMKilled_before_pg_failure');
});

test('event graph connects SQL fingerprint, slow log, plan, waits and blocking session with raw sources', () => {
  const logId = '55555555-5555-4555-8555-555555555555';
  const planId = '66666666-6666-4666-8666-666666666666';
  const event = { ...base, eventGroup: 'latency', evidence: [{ id: evidenceId,
    kind: 'alert_firing', observedAt: '2026-11-17T00:01:00Z',
    payload: { metric: 'pg.blocked_sessions', sampleId } }] };
  const sample = { id: sampleId, collectedAt: '2026-11-17T00:01:00Z', status: 'ok',
    metrics: { diagnostics: {
      activity: { status: 'ok', data: [{ state: 'active', waitType: 'Lock', sessions: 1 }] },
      lockChain: { status: 'ok', data: [{ waitingPid: 21, blockerPid: 10 }] },
      wal: { status: 'ok', data: { bytes: '4096' } }
    }, sqlPerformance: { status: 'ok', statements: [{ fingerprint: '1:2:42:1', queryId: '42' }] } } };
  const analysis = analyzeEvent(event, [sample], { nearbySamples: [sample],
    slowLogs: [{ id: logId, observedAt: sample.collectedAt, durationMs: 250, queryId: '42' }],
    plans: [{ id: planId, observedAt: sample.collectedAt, queryId: '42',
      summary: { nodeCount: 1 } }] });
  assert.ok(analysis.graph.edges.some(edge => edge.from === `session:${sampleId}:21` &&
    edge.to === `session:${sampleId}:10` && edge.relation === 'blocked_by'));
  assert.ok(analysis.graph.edges.some(edge => edge.from === `slow_log:${logId}` &&
    edge.to === `sql:${event.assetId}:1:2:42:1`));
  assert.ok(analysis.graph.edges.some(edge => edge.from === `plan:${planId}` &&
    edge.to === `sql:${event.assetId}:1:2:42:1`));
  assert.ok(analysis.graph.nodes.some(node => node.type === 'wait'));
  assert.ok(analysis.graph.nodes.some(node => node.type === 'wal'));
  assert.equal(analysis.graph.nodes.find(node => node.id === `slow_log:${logId}`).source,
    `/api/assets/${event.assetId}/slow-logs/${logId}`);
});

test('SQL regression is a time correlated hypothesis and autovacuum backlog is separately evidenced', () => {
  const ids = [sampleId, '77777777-7777-4777-8777-777777777777',
    '88888888-8888-4888-8888-888888888888'];
  const sql = (calls, totalExecMs) => ({ status: 'ok', resetAt: '2026-11-16T00:00:00Z',
    statements: [{ fingerprint: '1:2:42:1', queryId: '42', calls, totalExecMs,
      rows: 0, sharedBlocksHit: 0, sharedBlocksRead: 0, sharedBlocksWritten: 0,
      tempBlocksRead: 0, tempBlocksWritten: 0, walBytes: 0 }] });
  const samples = [[15, 800], [10, 200], [5, 100]].map(([calls, totalExecMs], i) => ({
    id: ids[i], collectedAt: `2026-11-17T00:0${3 - i}:00Z`, status: 'ok',
    metrics: { sqlPerformance: sql(calls, totalExecMs) }
  }));
  const event = { ...base, eventGroup: 'latency', evidence: [] };
  const analysis = analyzeEvent(event, samples, { nearbySamples: samples,
    slowLogs: [{ id: '55555555-5555-4555-8555-555555555555',
      queryId: '42', observedAt: samples[0].collectedAt, durationMs: 220 }] });
  const candidate = analysis.candidates.find(item => item.code === 'sql_regression');
  assert.equal(candidate.detail.ratio, 6);
  assert.equal(candidate.confidence, 'medium');
  assert.equal(candidate.state, 'hypothesis');
  const maintenance = { ...base, eventGroup: 'maintenance', evidence: [{ id: evidenceId,
    kind: 'alert_firing', observedAt: samples[0].collectedAt,
    payload: { sampleId, metric: 'pg.autovacuum_disabled', value: 1 } }] };
  const vacuumSample = { ...samples[0], metrics: { diagnostics: { vacuum: { status: 'ok',
    data: [{ schema: 'public', table: 'orders', deadRowsEstimate: 20000 }] } } } };
  const vacuumAnalysis = analyzeEvent(maintenance, [vacuumSample]);
  assert.equal(vacuumAnalysis.candidates.find(item => item.code === 'autovacuum_disabled').confidence,
    'medium');
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
  const source = detail.analysis.graph.nodes.find(node => node.type === 'sample')?.source;
  assert.ok(source);
  assert.equal((await fetch(baseUrl + source)).status, 401);
  const raw = await fetch(baseUrl + source, { headers: { cookie } });
  assert.equal(raw.status, 200);
  assert.equal((await raw.json()).sample.assetId, asset.id);
});
