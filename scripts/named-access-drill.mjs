import assert from 'node:assert/strict';

const base = process.env.DBSPHERE_ACCESS_TEST_URL;
const adminPassword = process.env.DBSPHERE_ADMIN_PASSWORD;
const legacyPassword = process.env.DBSPHERE_VIEWER_PASSWORD;
assert.ok(base && adminPassword && legacyPassword, 'A dedicated DbSphere test instance with a legacy viewer is required');
const testPassword = 'access-drill-initial-only';
const replacement = 'access-drill-replacement-only';

async function request(session, method, path, expected, data) {
  const headers = { 'content-type': 'application/json' };
  if (session) Object.assign(headers, { cookie: session.cookie, 'x-csrf-token': session.csrf });
  const response = await fetch(base + path, { method, headers, body: JSON.stringify(data) });
  const body = await response.text();
  assert.equal(response.status, expected, `${method} ${path}: ${response.status} ${body.slice(0, 500)}`);
  return { response, body, json: () => JSON.parse(body) };
}

async function login(username, password, role) {
  const { response, json } = await request(null, 'POST', '/api/login', 200, { username, password });
  const session = json();
  assert.equal(session.role, role);
  if (username) assert.equal(session.username, username);
  return { cookie: response.headers.get('set-cookie').split(';')[0], csrf: session.csrf };
}

const admin = await login('', adminPassword, 'admin');
const seed = Date.now().toString(36);
const accountName = suffix => `drill-${suffix}-${seed}`;
async function asset(label, port) {
  const { json } = await request(admin, 'POST', '/api/assets', 201, { name: `Access ${label} ${seed}`,
    host: '127.0.0.1', port, database: 'postgres', username: 'monitor', password: 'disposable-test-secret',
    sslMode: 'disable', runtime: 'linux' });
  const id = json().asset.id;
  const check = await request(admin, 'POST', `/api/assets/${id}/check`, 200);
  assert.equal(check.json().result.status, 'error');
  const events = await request(admin, 'GET', '/api/events', 200);
  const event = events.json().events.find(item => item.assetId === id && item.status === 'open');
  assert.ok(event, `No incident for ${label}`);
  const detail = await request(admin, 'GET', `/api/events/${event.id}`, 200);
  const source = detail.json().event.evidence.find(item => item.kind === 'collection_error');
  assert.ok(source?.payload?.sampleId);
  return { id, event: event.id, sample: source.payload.sampleId };
}

const a = await asset('A', 1);
const b = await asset('B', 2);
async function team(project, assetId) {
  const result = await request(admin, 'POST', '/api/admin/teams', 201, {
    project, name: seed, assetIds: [assetId]
  });
  return result.json().team.id;
}
const teamA = await team('Production A', a.id);
const teamB = await team('Production B', b.id);
async function account(suffix, role, teamIds, legacyRole) {
  const username = accountName(suffix);
  const result = await request(admin, 'POST', '/api/admin/accounts', 201, {
    username, role, teamIds, legacyRole, password: legacyRole ? undefined : testPassword
  });
  assert.doesNotMatch(result.body, /password|hash/i);
  return result.json().account;
}
const operator = await account('operator', 'operator', [teamA]);
const onCall = await account('oncall', 'operator', [teamA]);
const otherOperator = await account('other-operator', 'operator', [teamB]);
const approver = await account('approver', 'approver', [teamA]);
const otherApprover = await account('other-approver', 'approver', [teamB]);
const viewer = await account('viewer', 'viewer', [teamA], 'viewer');
const empty = await account('empty', 'viewer', []);
const namedAdmin = await account('admin', 'admin', []);
const namedAdminSession = await login(namedAdmin.username, testPassword, 'admin');
const opSession = await login(operator.username, testPassword, 'operator');
const confirm = await login(approver.username, testPassword, 'approver');
const confirmOther = await login(otherApprover.username, testPassword, 'approver');
const read = await login(viewer.username, legacyPassword, 'viewer');
const readNone = await login(empty.username, testPassword, 'viewer');
const legacy = await login('', legacyPassword, 'viewer');

const ev = `/api/events/${a.event}`;
const evOther = `/api/events/${b.event}`;
const raw = (asset, sample = asset.sample) => `/api/assets/${asset.id}/samples/${sample}`;
const directory = await request(namedAdminSession, 'GET', '/api/admin/access', 200);
assert.ok(directory.json().accounts.some(item => item.username === operator.username));
assert.doesNotMatch(directory.body, /passwordHash|password_hash/);
assert.equal((await request(legacy, 'GET', evOther, 200)).json().event.id, b.event);

for (const session of [opSession, confirm, read]) {
  for (const path of ['/api/access/assets/export', '/api/assets/page?limit=10', '/api/events/page?limit=10']) {
    const { body } = await request(session, 'GET', path, 200);
    assert.ok(body.includes(a.id) && !body.includes(b.id), path);
  }
  for (const path of [ev, `${ev}/report`, raw(a)]) await request(session, 'GET', path, 200);
  for (const path of [evOther, `${evOther}/report`, raw(b), raw(a, b.sample)]) await request(session, 'GET', path, 404);
  await request(session, 'GET', '/api/admin/access', 403);
}
for (const path of ['/api/access/assets/export', '/api/assets', '/api/events', '/api/assets/page?limit=10', '/api/events/page?limit=10']) {
  const { body } = await request(readNone, 'GET', path, 200);
  assert.ok(!body.includes(a.id) && !body.includes(b.id), `empty grant: ${path}`);
}
await request(read, 'POST', `${ev}/ack`, 403);
await request(confirm, 'POST', `${ev}/ack`, 403);
await request(opSession, 'POST', `${evOther}/ack`, 404);
await request(opSession, 'POST', `${ev}/ack`, 200);
await request(opSession, 'POST', `${ev}/response`, 200, { phase: 'diagnosis', note: 'Validated original PostgreSQL failure sample' });
await request(opSession, 'POST', `${ev}/handoff`, 404, { accountId: otherOperator.id, note: 'Out of scope assignment' });
await request(opSession, 'POST', `${ev}/handoff`, 200, { accountId: onCall.id, note: 'Investigate original connection failure' });
assert.equal((await request(opSession, 'GET', `${ev}/handoff`, 200)).json().assignment.username, onCall.username);

const { json: orderJSON } = await request(opSession, 'POST', `${ev}/work-orders`, 201, { reason: 'Resample the original connection failure' });
const order = orderJSON().order;
assert.equal(order.requestedBy, operator.username);
const decisionPath = `/api/work-orders/${order.id}/approve`;
await request(opSession, 'POST', decisionPath, 403);
await request(confirmOther, 'POST', decisionPath, 404);
await request(admin, 'PATCH', `/api/admin/accounts/${operator.id}`, 200, { role: 'approver' });
await request(opSession, 'POST', decisionPath, 409);
const approved = (await request(confirm, 'POST', decisionPath, 200)).json().order;
assert.equal(approved.approvedBy, approver.username);
assert.equal(approved.status, 'approved');
await request(admin, 'PATCH', `/api/admin/accounts/${operator.id}`, 200, { role: 'operator' });

const detail = (await request(read, 'GET', ev, 200)).json().event;
for (const [kind, actor] of [['event_acknowledged', operator.username], ['operator_response', operator.username],
  ['event_handoff', operator.username], ['work_order_requested', operator.username], ['work_order_decision', approver.username]]) {
  assert.ok(detail.evidence.some(item => item.kind === kind && item.payload.actor === actor), `${kind} evidence lost actor`);
}
const audit = await request(namedAdminSession, 'GET', `${ev}/audit`, 200);
assert.ok(audit.body.includes(operator.username) && audit.body.includes(approver.username));
await request(namedAdminSession, 'POST', `/api/admin/accounts/${viewer.id}/rotate`, 200, { password: replacement });
await request(read, 'GET', ev, 401);
await request(null, 'POST', '/api/login', 401, { username: viewer.username, password: legacyPassword });
const rotated = await login(viewer.username, replacement, 'viewer');
await request(rotated, 'GET', ev, 200);
await request(rotated, 'POST', '/api/logout', 200);
await request(rotated, 'GET', ev, 401);
await request(admin, 'PATCH', `/api/admin/accounts/${empty.id}`, 200, { enabled: false });
await request(readNone, 'GET', '/api/assets/page?limit=10', 401);

if (process.env.DBSPHERE_ACCESS_TEST_DETAILS === '1') {
  console.log(JSON.stringify({ asset: a.id, event: a.event, otherEvent: b.event, sourceSample: a.sample,
    operator: operator.username, viewer: viewer.username, approver: approver.username, onCall: onCall.username,
    admin: namedAdmin.username, order: order.id }));
}
console.log('Named accounts, scope, incident and original evidence, independent approval, migration, rotation and logout verified');
