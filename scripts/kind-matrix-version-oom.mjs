#!/usr/bin/env node

// Test-only administrator fault injection. DbSphere keeps its read-only
// ServiceAccount while PG14–17 each demonstrate a real Pod OOM incident.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';

const [baseURL, credentialsPath, kubeconfigPath, resultPath] = process.argv.slice(2);
if (!baseURL || !credentialsPath || !kubeconfigPath || !resultPath) {
  console.error('usage: node scripts/kind-matrix-version-oom.mjs DBSPHERE_URL CREDENTIALS KUBECONFIG RESULT');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const credentials = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const namespace = 'dbsphere-matrix';
let cookie = '';
let csrf = '';
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
const kube = (args, timeout = 25000) => execFileSync('kubectl', [
  '--kubeconfig', kubeconfigPath, '--request-timeout=180s', '-n', namespace, ...args
], { encoding: 'utf8', timeout }).trim();
const pods = app => JSON.parse(kube(['get', 'pods', '-l', `app=${app}`, '-o', 'json'])).items;

async function api(path, method = 'GET', payload) {
  const response = await fetch(base + path, {
    method,
    headers: {
      ...(cookie ? { Cookie: cookie } : {}),
      ...(csrf && method !== 'GET' ? { 'X-CSRF-Token': csrf } : {}),
      ...(payload ? { 'Content-Type': 'application/json' } : {})
    },
    body: payload ? JSON.stringify(payload) : undefined,
    signal: AbortSignal.timeout(45000)
  });
  const body = await response.json();
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${JSON.stringify(body)}`);
  return body;
}
async function until(label, attempts, read) {
  for (let i = 0; i < attempts; i++) {
    const value = await read();
    if (value) return value;
    await pause(1000);
  }
  throw new Error(`${label} was not observed after ${attempts} attempts`);
}
async function waitGone(app) {
  await until(`${app} old Pod deletion`, 90, () => pods(app).length === 0);
}
function resources(app, requestMemory, limitMemory) {
  kube(['set', 'resources', `deployment/${app}`, '--containers=postgres',
    `--requests=cpu=25m,memory=${requestMemory}`, `--limits=memory=${limitMemory}`]);
}
function faultCommand(app, enabled) {
  // Runtime size prevents Perl's constant folding from allocating before
  // containerd installs the OOM event watcher.
  const container = enabled
    ? { name: 'postgres', command: ['/usr/bin/perl'],
        args: ['-e', 'my $size = 128*1024*1024; sleep 5; my $x = "x" x $size; sleep 60;'] }
    : { name: 'postgres', command: null, args: null };
  kube(['patch', `deployment/${app}`, '--type=strategic', '-p',
    JSON.stringify({ spec: { template: { spec: { containers: [container] } } } })]);
}
async function restore(app) {
  kube(['scale', `deployment/${app}`, '--replicas=0']);
  await waitGone(app);
  faultCommand(app, false);
  resources(app, '64Mi', '512Mi');
  kube(['scale', `deployment/${app}`, '--replicas=1']);
  kube(['rollout', 'status', `deployment/${app}`, '--timeout=180s'], 200000);
}
function diagnose(app) {
  for (const pod of pods(app)) {
    console.error(JSON.stringify({ name: pod.metadata.name, phase: pod.status?.phase,
      containers: pod.spec?.containers?.map(container => ({ name: container.name,
        command: container.command, args: container.args, resources: container.resources })),
      statuses: pod.status?.containerStatuses?.map(status => ({ state: status.state,
        lastState: status.lastState, restarts: status.restartCount })) }));
    try { console.error(`previous ${pod.metadata.name}: ${kube([
      'logs', pod.metadata.name, '-c', 'postgres', '--previous', '--tail=20'
    ])}`); } catch (error) { console.error(`previous logs unavailable: ${error.message}`); }
  }
}

assert.equal((await api('/readyz')).storage, 'postgresql18');
const login = await fetch(base + '/api/login', {
  method: 'POST', headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ password: credentials.adminPassword }),
  signal: AbortSignal.timeout(10000)
});
assert.equal(login.status, 200);
cookie = login.headers.get('set-cookie')?.split(';')[0] || '';
csrf = (await login.json()).csrf;
assert.ok(cookie && csrf);
const assets = (await api('/api/assets')).assets;
const results = [];

for (const major of [14, 15, 16, 17]) {
  const app = `pg${major}`;
  const asset = assets.find(item => item.name === `kind-matrix-${app}`);
  assert.ok(asset?.id, `${app} monitored asset missing`);
  const baselinePod = pods(app)[0];
  assert.equal(baselinePod?.status?.phase, 'Running');
  const baseline = (await api(`/api/assets/${asset.id}/check`, 'POST')).result;
  assert.equal(baseline.status, 'ok', `${app} baseline collection failed`);

  let restored = false;
  try {
    kube(['scale', `deployment/${app}`, '--replicas=0']);
    await waitGone(app);
    resources(app, '8Mi', '32Mi');
    faultCommand(app, true);
    kube(['scale', `deployment/${app}`, '--replicas=1']);
    let failedPod;
    try {
      failedPod = await until(`${app} real OOMKilled`, 120, () => pods(app).find(pod =>
        pod.status?.containerStatuses?.some(status =>
          status.lastState?.terminated?.reason === 'OOMKilled')));
    } catch (error) { diagnose(app); throw error; }
    assert.notEqual(failedPod.metadata.uid, baselinePod.metadata.uid);

    const failure = await until(`${app} database/Kubernetes failure sample`, 30, async () => {
      const check = (await api(`/api/assets/${asset.id}/check`, 'POST')).result;
      const sample = (await api(`/api/assets/${asset.id}/samples/${check.sampleId}`)).sample;
      return check.status === 'error' && JSON.stringify(sample.metrics?.k8s ?? {}).includes('OOMKilled')
        ? { check, sample } : null;
    });
    const incident = await until(`${app} OOM candidate in one event`, 30, async () => {
      const open = (await api('/api/events')).events.find(item =>
        item.assetId === asset.id && item.status === 'open');
      if (!open) return null;
      const detail = await api(`/api/events/${open.id}`);
      return detail.analysis?.candidates?.some(candidate =>
        candidate.code === 'pod_oom_correlated' && candidate.supports?.length)
        ? detail : null;
    });
    assert.equal(incident.event.assetId, asset.id);

    await restore(app);
    restored = true;
    const recovery = await until(`${app} same-event recovery`, 30, async () => {
      const check = (await api(`/api/assets/${asset.id}/check`, 'POST')).result;
      const detail = await api(`/api/events/${incident.event.id}`);
      return check.status === 'ok' && detail.event.status === 'resolved'
        ? { check, detail } : null;
    });
    results.push({ major, assetId: asset.id, eventId: incident.event.id,
      failedPod: failedPod.metadata.name, failedPodNode: failedPod.spec.nodeName,
      failedSampleId: failure.check.sampleId, recoveredSampleId: recovery.check.sampleId,
      rootCauseCandidate: 'pod_oom_correlated', status: recovery.detail.event.status });
  } finally {
    if (!restored) await restore(app);
  }
}

writeFileSync(resultPath, JSON.stringify({ versions: results }, null, 2) + '\n');
console.log(JSON.stringify({ versions: results }, null, 2));
