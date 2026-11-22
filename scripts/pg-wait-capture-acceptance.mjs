import assert from 'node:assert/strict';
import { writeFile } from 'node:fs/promises';
import pg from 'pg';

const base = process.env.DBSPHERE_TEST_BASE;
const out = process.env.DBSPHERE_WAIT_CAPTURE_PROOF;
const major = Number(process.env.DBSPHERE_TEST_PG_MAJOR);
const host = process.env.DBSPHERE_TEST_PG_HOST || '127.0.0.1';
const port = Number(process.env.DBSPHERE_TEST_PG_PORT || 15432);
const credentials = { host, port, database: 'postgres', user: 'postgres',
  password: process.env.DBSPHERE_TEST_PG_TARGET_PASSWORD || 'dbsphere-root-test', query_timeout: 90000 };
const monitorPassword = process.env.DBSPHERE_TEST_PG_PASSWORD || 'dbsphere-monitor-test';
assert.ok(base && out && process.env.DBSPHERE_ADMIN_PASSWORD && major);

let cookie, csrf;
async function request(path, method = 'GET', body, expected = 200) {
  const response = await fetch(base + path, { method, headers: {
    ...(cookie ? { cookie } : {}), ...(csrf ? { 'X-CSRF-Token': csrf } : {}),
    ...(body === undefined ? {} : { 'Content-Type': 'application/json' })
  }, body: body === undefined ? undefined : JSON.stringify(body), signal: AbortSignal.timeout(25000) });
  const result = await response.json();
  assert.equal(response.status, expected, `${method} ${path}: ${JSON.stringify(result).slice(0, 300)}`);
  if (path === '/api/login') cookie = response.headers.get('set-cookie').split(';')[0];
  return result;
}
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
const clients = [];
async function connect(application_name) {
  const client = new pg.Client({ ...credentials, application_name });
  clients.push(client);
  await client.connect();
  return client;
}
async function until(observe, reason) {
  for (let attempt = 0; attempt < 40; attempt++) {
    const value = await observe();
    if (value) return value;
    await sleep(100);
  }
  assert.fail(reason);
}
const pid = async client => (await client.query('SELECT pg_backend_pid() pid')).rows[0].pid;
const route = asset => `/api/assets/${asset.id}`;
const capturePath = asset => `${route(asset)}/wait-captures`;
const link = (edge, waiting, blocker) => edge.waitingPid === waiting && edge.blockerPid === blocker;
async function addAsset(name, username = 'dbsphere_monitor', password = monitorPassword) {
  const assetHost = process.env.DBSPHERE_TEST_PG_ASSET_HOST || host;
  const assetPort = Number(process.env.DBSPHERE_TEST_PG_ASSET_PORT || port);
  return (await request('/api/assets', 'POST', { name, host: assetHost, port: assetPort, database: 'postgres', username, password,
    sslMode: 'disable', runtime: 'linux' }, 201)).asset;
}

csrf = (await request('/api/login', 'POST', { password: process.env.DBSPHERE_ADMIN_PASSWORD })).csrf;
await request('/api/alert-rules/pg_lock_wait', 'PATCH', { durationSeconds: 0 });
const admin = await connect('wait-capture-observer');
const root = await connect('wait-capture-root');
const middle = await connect('wait-capture-middle');
const leaf = await connect('wait-capture-leaf');
const rootPID = await pid(root), middlePID = await pid(middle), leafPID = await pid(leaf);
let blockedMiddle, blockedLeaf;
let loadEnabled = true;
const background = [];
try {
  await root.query('BEGIN; SELECT pg_advisory_xact_lock(51268701)');
  await middle.query('BEGIN; SELECT pg_advisory_xact_lock(51268702)');
  blockedMiddle = middle.query('SELECT pg_advisory_xact_lock(51268701)').catch(error => error);
  blockedLeaf = leaf.query('SELECT pg_advisory_xact_lock(51268702)').catch(error => error);
  await until(async () => {
    const { rows } = await admin.query('SELECT pg_blocking_pids($1) a, pg_blocking_pids($2) b', [middlePID, leafPID]);
    return rows[0].a.includes(rootPID) && rows[0].b.includes(middlePID);
  }, 'Could not create a two-hop PostgreSQL lock chain');

  const asset = await addAsset(`Wait capture PG${major}`);
  const check = (await request(`${route(asset)}/check`, 'POST')).result;
  const event = (await request('/api/events')).events.find(row => row.assetId === asset.id && row.status === 'open');
  assert.ok(event?.id && check.sampleId, 'The real lock chain did not open an incident');
  const payload = { profile: 'standard', eventId: event.id };
  const standardPending = request(capturePath(asset), 'POST', payload, 201);
  await until(async () => (await admin.query("SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE application_name='dbsphere-wait-capture') found")).rows[0].found,
    'The capture did not open its dedicated connection');
  await request(capturePath(asset), 'POST', payload, 429);
  const standard = await standardPending;
  await request(capturePath(asset), 'POST', payload, 429);
  assert.equal(standard.eventId, event.id);
  assert.equal(standard.result.intervalMs, 1000);
  assert.equal(standard.result.expected, 10);

  function lockFrame(capture) {
    const frame = capture.result.frames.find(item => item.snapshot?.edges.some(edge => link(edge, middlePID, rootPID)) &&
      item.snapshot.edges.some(edge => link(edge, leafPID, middlePID)));
    assert.ok(frame, `${capture.result.profile} missed the real two-hop lock chain`);
    assert.equal(frame.snapshot.visibility, 'full');
    const blocker = frame.snapshot.sessions.find(session => session.pid === rootPID);
    assert.ok(blocker?.xactStart && blocker.backendStart && frame.snapshot.serverStartedAt);
    return frame;
  }
  const initialFrame = lockFrame(standard);
  const proof = { major, asset: asset.id, event: event.id, sourceSample: check.sampleId, standard,
    session: { rootPID, middlePID, leafPID, rootStart: initialFrame.snapshot.sessions.find(s => s.pid === rootPID).backendStart } };
  if (major === 18) {
    const enhancedAsset = await addAsset('Enhanced wait capture under load');
    await request(`${route(enhancedAsset)}/check`, 'POST');
    const enhancedEvent = (await request('/api/events')).events.find(row => row.assetId === enhancedAsset.id && row.status === 'open');
    assert.ok(enhancedEvent?.id);
    await request(capturePath(enhancedAsset), 'POST', payload, 404);
    const loadClients = await Promise.all(Array.from({ length: 6 }, (_, i) => connect(`wait-capture-short-${i}`)));
    for (const client of loadClients) background.push((async () => {
      while (loadEnabled) await client.query('SELECT pg_sleep(0.045)');
    })());
    const parallel = await connect('wait-capture-parallel');
    await admin.query(`CREATE TABLE dbsphere_wait_parallel_input AS SELECT n FROM generate_series(1,300) n;
      ALTER TABLE dbsphere_wait_parallel_input SET (parallel_workers=2);
      CREATE FUNCTION dbsphere_wait_parallel_delay(n int) RETURNS boolean LANGUAGE plpgsql PARALLEL SAFE AS
        $$ BEGIN PERFORM pg_sleep(0.075); RETURN n>0; END; $$`);
    await parallel.query(`SET min_parallel_table_scan_size=0; SET parallel_setup_cost=0; SET parallel_tuple_cost=0;
      SET max_parallel_workers_per_gather=2`);
    const parallelPID = await pid(parallel);
    background.push(parallel.query('SELECT count(*) FROM dbsphere_wait_parallel_input WHERE dbsphere_wait_parallel_delay(n)'));
    await until(async () => (await admin.query('SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE leader_pid=$1) found', [parallelPID])).rows[0].found,
      'The parallel fixture did not launch real worker processes');
    proof.enhanced = await request(capturePath(enhancedAsset), 'POST', { profile: 'enhanced', eventId: enhancedEvent.id }, 201);
    loadEnabled = false;
    await Promise.all(background);
    assert.equal(proof.enhanced.result.intervalMs, 250);
    assert.equal(proof.enhanced.result.expected, 40);
    assert.ok(proof.enhanced.result.observed >= 30);
    lockFrame(proof.enhanced);
    assert.ok(proof.enhanced.result.frames.some(frame => frame.snapshot?.sessions.some(s => s.leaderPid === parallelPID && s.backendType === 'parallel worker')),
      'Enhanced capture lost the parallel worker identity');
    assert.ok(proof.enhanced.result.frames.some(frame => frame.snapshot?.sessions.some(s => s.waitEvent === 'PgSleep' && s.leaderPid === null)));
    assert.ok(proof.enhanced.result.frames.filter(frame => frame.snapshot?.lockObserved).length <= 10);
    assert.doesNotMatch(JSON.stringify(proof.enhanced.result), /SELECT pg_sleep|dbsphere_wait_parallel_delay/);
    const otherAsset = await addAsset('Limited wait visibility', 'dbsphere_wait_limited', 'dbsphere-limited-test');
    await admin.query("CREATE ROLE dbsphere_wait_limited LOGIN PASSWORD 'dbsphere-limited-test'");
    const limited = await request(capturePath(otherAsset), 'POST', { profile: 'standard' }, 201);
    assert.ok(limited.result.frames.some(frame => frame.snapshot?.visibility === 'limited'));
    assert.ok(limited.result.frames.filter(frame => frame.snapshot).every(frame => !frame.snapshot.lockObserved && !frame.snapshot.sessions.length));
    proof.limited = { observed: limited.result.observed, expected: limited.result.expected };
    await request(`${capturePath(otherAsset)}?id=${standard.id}`, 'GET', undefined, 404);
    proof.enhancedAsset = enhancedAsset.id;
    proof.enhancedEvent = enhancedEvent.id;
  }

  await root.query('ROLLBACK');
  assert.ok(!((await blockedMiddle) instanceof Error));
  await middle.query('ROLLBACK');
  assert.ok(!((await blockedLeaf) instanceof Error));
  await request(`${route(asset)}/check`, 'POST');
  const resolved = await request(`/api/events/${event.id}`);
  assert.equal(resolved.event.status, 'resolved');
  assert.equal(resolved.waitCaptures[0].id, standard.id);
  assert.ok(resolved.analysis.graph.nodes.some(node => node.id === `wait-capture:${standard.id}`));
  const replay = await request(`${capturePath(asset)}?id=${standard.id}`);
  assert.deepEqual(replay.result, standard.result);
  proof.eventStatus = resolved.event.status;
  await writeFile(out, JSON.stringify(proof, null, 2));
  console.log(JSON.stringify({ major, event: event.id, standard: { observed: standard.result.observed, queryTotalMs: standard.result.queryTotalMs, queryMaximumMs: standard.result.queryMaximumMs },
    enhanced: proof.enhanced && { observed: proof.enhanced.result.observed, queryTotalMs: proof.enhanced.result.queryTotalMs, queryMaximumMs: proof.enhanced.result.queryMaximumMs } }));
} finally {
  loadEnabled = false;
  await root.query('ROLLBACK').catch(() => {});
  await blockedMiddle;
  await middle.query('ROLLBACK').catch(() => {});
  await blockedLeaf;
  await Promise.allSettled(background);
  await Promise.allSettled(clients.map(client => client.end()));
}
