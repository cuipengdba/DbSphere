#!/usr/bin/env node

// An isolated, real application-to-PostgreSQL Pod OOM drill in the four-node
// kind matrix. Fault injection uses the test administrator's kubeconfig;
// DbSphere itself continues to use its read-only ServiceAccount.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';

const [baseURL, appURL, sdkURL, credentialsPath, kubeconfigPath, resultPath] = process.argv.slice(2);
if (!baseURL || !appURL || !sdkURL || !credentialsPath || !kubeconfigPath || !resultPath) {
  console.error('usage: node scripts/kind-matrix-app-oom.mjs DBSPHERE_URL APP_URL SDK_URL CREDENTIALS KUBECONFIG RESULT');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const app = appURL.replace(/\/$/, '');
const sdk = sdkURL.replace(/\/$/, '');
const credentials = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const namespace = 'dbsphere-matrix';
let cookie = '';
let csrf = '';
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
const kube = (args, timeout = 25000) => execFileSync('kubectl', [
  '--kubeconfig', kubeconfigPath, '--request-timeout=180s', '-n', namespace, ...args
], { encoding: 'utf8', timeout }).trim();
const pods = selector => JSON.parse(kube(['get', 'pods', '-l', `app=${selector}`, '-o', 'json'])).items;

async function api(path, method = 'GET', payload, allowNotFound = false) {
  const response = await fetch(base + path, {
    method,
    headers: {
      ...(cookie ? { Cookie: cookie } : {}),
      ...(method !== 'GET' && csrf ? { 'X-CSRF-Token': csrf } : {}),
      ...(payload ? { 'Content-Type': 'application/json' } : {})
    },
    body: payload ? JSON.stringify(payload) : undefined,
    signal: AbortSignal.timeout(45000)
  });
  const body = await response.json();
  if (allowNotFound && response.status === 404) return null;
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${JSON.stringify(body)}`);
  return body;
}
async function checkout(endpoint) {
  const response = await fetch(endpoint + '/checkout', { signal: AbortSignal.timeout(55000) });
  const body = await response.json();
  assert.match(body.traceId || '', /^[0-9a-f]{32}$/);
  return { code: response.status, body };
}
async function until(label, attempts, delay, read) {
  let last;
  for (let index = 0; index < attempts; index++) {
    last = await read();
    if (last) return last;
    await pause(delay);
  }
  throw new Error(`${label} was not observed after ${attempts} attempts`);
}
async function waitPodsGone() {
  await until('old PostgreSQL Pod deletion', 90, 1000,
    () => pods('pg18').length === 0);
}
function resources(requestMemory, limitMemory) {
  kube(['set', 'resources', 'deployment/pg18', '--containers=postgres',
    `--requests=cpu=25m,memory=${requestMemory}`, `--limits=memory=${limitMemory}`]);
}
function faultCommand(enabled) {
  // The official postgres:18-bookworm image contains Perl. Make the container's
  // PID 1 grows memory in 4 MiB steps after the OOM watcher is installed.
  // A variable prevents Perl from allocating at compile time.
  // This is a test-only Deployment patch, restored before PostgreSQL recovery.
  const container = enabled
    ? { name: 'postgres', command: ['/usr/bin/perl'],
        args: ['-e', 'my @blocks; my $chunk=4*1024*1024; sleep 5; while (1) { push @blocks, "x" x $chunk; sleep 1; }'] }
    : { name: 'postgres', command: null, args: null };
  kube(['patch', 'deployment/pg18', '--type=strategic', '-p',
    JSON.stringify({ spec: { template: { spec: { containers: [container] } } } })]);
}
async function restore() {
  kube(['scale', 'deployment/pg18', '--replicas=0']);
  await waitPodsGone();
  faultCommand(false);
  resources('64Mi', '512Mi');
  kube(['scale', 'deployment/pg18', '--replicas=1']);
  kube(['rollout', 'status', 'deployment/pg18', '--timeout=180s'], 200000);
}

const nodes = JSON.parse(execFileSync('kubectl', ['--kubeconfig', kubeconfigPath,
  'get', 'nodes', '-o', 'json'], { encoding: 'utf8', timeout: 25000 })).items;
assert.ok(nodes.length >= 4, 'four-node kind cluster is required');
const appPod = pods('dbsphere-oom-app')[0];
const sdkPod = pods('dbsphere-sdk-oom-app')[0];
const targetPod = pods('pg18')[0];
assert.ok(appPod?.status?.phase === 'Running' && sdkPod?.status?.phase === 'Running' &&
  targetPod?.status?.phase === 'Running');
assert.notEqual(appPod.spec.nodeName, targetPod.spec.nodeName, 'application and PG must span nodes');
assert.notEqual(sdkPod.spec.nodeName, targetPod.spec.nodeName, 'SDK application and PG must span nodes');
assert.equal((await api('/readyz')).storage, 'postgresql18');
const loginResponse = await fetch(base + '/api/login', {
  method: 'POST', headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ password: credentials.adminPassword }),
  signal: AbortSignal.timeout(10000)
});
assert.equal(loginResponse.status, 200);
cookie = loginResponse.headers.get('set-cookie')?.split(';')[0] || '';
csrf = (await loginResponse.json()).csrf;
assert.ok(cookie && csrf);
const asset = (await api('/api/assets')).assets.find(item => item.name === 'kind-matrix-pg18');
assert.ok(asset?.id, 'PG18 kind asset was not created by the matrix acceptance');
const baseline = await checkout(app);
assert.equal(baseline.code, 200, 'baseline application request failed');
const baselineTrace = await api(`/api/assets/${asset.id}/traces/${baseline.body.traceId}`);
assert.equal(baselineTrace.spans?.length, 2, 'baseline application Trace lacks parent and DB Span');
const sdkBaseline = await checkout(sdk);
assert.equal(sdkBaseline.code, 200, 'official SDK baseline request failed');
const sdkBaselineTrace = await until('official SDK baseline parent and DB Span', 25, 200, async () => {
  const raw = await api(`/api/assets/${asset.id}/traces/${sdkBaseline.body.traceId}`,
    'GET', undefined, true);
  return raw?.spans?.length === 2 && raw.spans.some(span =>
    span.kind === 'postgresql_client' && span.appPodVerified?.uid === sdkPod.metadata.uid)
    ? raw : null;
});
const baselineSample = (await api(`/api/assets/${asset.id}/check`, 'POST')).result;
assert.equal(baselineSample.status, 'ok');

let restored = false;
let result;
try {
  kube(['scale', 'deployment/pg18', '--replicas=0']);
  await waitPodsGone();
  resources('8Mi', '32Mi');
  faultCommand(true);
  kube(['scale', 'deployment/pg18', '--replicas=1']);
  let failedPod;
  try {
    failedPod = await until('real OOMKilled PostgreSQL container', 120, 1000, () =>
      pods('pg18').find(pod => pod.status?.containerStatuses?.some(status =>
        status.lastState?.terminated?.reason === 'OOMKilled')));
  } catch (error) {
    console.error(JSON.stringify(pods('pg18').map(pod => ({ name: pod.metadata.name,
      phase: pod.status?.phase, containers: pod.spec?.containers?.map(container => ({
        name: container.name, command: container.command, args: container.args,
        resources: container.resources
      })), statuses: pod.status?.containerStatuses?.map(status => ({
        state: status.state, lastState: status.lastState, restarts: status.restartCount
      })) }))));
    for (const pod of pods('pg18')) {
      try { console.error(`previous ${pod.metadata.name}: ${kube([
        'logs', pod.metadata.name, '-c', 'postgres', '--previous', '--tail=20'
      ])}`); } catch (logsError) { console.error(`previous logs unavailable: ${logsError.message}`); }
    }
    throw error;
  }
  assert.notEqual(failedPod.metadata.uid, targetPod.metadata.uid);
  const failedRequest = await until('application connection failure', 20, 1000, async () => {
    const request = await checkout(app);
    return request.code === 502 && request.body.status === 'error' ? request : null;
  });
  const failedSDKRequest = await until('official SDK application connection failure', 20, 1000, async () => {
    const request = await checkout(sdk);
    return request.code === 502 && request.body.status === 'error' ? request : null;
  });
  const sample = await until('PostgreSQL and Kubernetes OOM sample', 20, 1000, async () => {
    const check = (await api(`/api/assets/${asset.id}/check`, 'POST')).result;
    const raw = (await api(`/api/assets/${asset.id}/samples/${check.sampleId}`)).sample;
    return check.status === 'error' && JSON.stringify(raw.metrics?.k8s).includes('OOMKilled')
      ? { check, raw } : null;
  });
  const event = await until('one incident with application and OOM evidence', 25, 1000, async () => {
    const open = (await api('/api/events')).events.find(item =>
      item.assetId === asset.id && item.status === 'open');
    if (!open) return null;
    const detail = await api(`/api/events/${open.id}`);
    const trace = await api(`/api/assets/${asset.id}/traces/${failedRequest.body.traceId}`);
    const sdkTrace = await api(`/api/assets/${asset.id}/traces/${failedSDKRequest.body.traceId}`,
      'GET', undefined, true);
    const candidates = detail.analysis?.candidates || [];
    const edges = detail.analysis?.graph?.edges || [];
    if (detail.event.id !== open.id ||
        !candidates.some(candidate => candidate.code === 'pod_oom_correlated' && candidate.supports?.length) ||
        edges.filter(edge => edge.relation === 'parent_of').length < 2 ||
        detail.analysis?.traceCoverage?.appPodVerified < 1 ||
        !trace.spans?.some(span => span.kind === 'postgresql_client' && span.status === 'error') ||
        !trace.spans?.some(span => span.kind === 'service' && span.status === 'error') ||
        !sdkTrace?.spans?.some(span => span.kind === 'postgresql_client' && span.status === 'error' &&
          span.appPodVerified?.uid === sdkPod.metadata.uid) ||
        !sdkTrace?.spans?.some(span => span.kind === 'service' && span.status === 'error')) return null;
    return { detail, trace, sdkTrace };
  });
  const incidentID = event.detail.event.id;
  const reportResponse = await fetch(`${base}/api/events/${incidentID}/report?format=md`, {
    headers: { Cookie: cookie }, signal: AbortSignal.timeout(20000)
  });
  assert.equal(reportResponse.status, 200);
  const report = await reportResponse.text();
  assert.ok(report.includes(incidentID) && report.includes('pod_oom_correlated') &&
    report.includes(failedRequest.body.traceId) && report.includes(failedSDKRequest.body.traceId),
  'report lost same-event OOM/Trace evidence');

  await restore();
  restored = true;
  const resolved = await until('same incident recovery', 30, 1000, async () => {
    const check = (await api(`/api/assets/${asset.id}/check`, 'POST')).result;
    const detail = await api(`/api/events/${incidentID}`);
    return check.status === 'ok' && detail.event.status === 'resolved' ? { check, detail } : null;
  });
  const recoveredRequest = await checkout(app);
  assert.equal(recoveredRequest.code, 200, 'application did not recover with PostgreSQL');
  const recoveredSDKRequest = await checkout(sdk);
  assert.equal(recoveredSDKRequest.code, 200, 'SDK application did not recover with PostgreSQL');
  result = {
    eventId: incidentID, assetId: asset.id, failedTraceId: failedRequest.body.traceId,
    sdkFailedTraceId: failedSDKRequest.body.traceId,
    failedSampleId: sample.check.sampleId, recoveredSampleId: resolved.check.sampleId,
    status: resolved.detail.event.status, rootCauseCandidate: 'pod_oom_correlated',
    failedPod: failedPod.metadata.name, failedPodNode: failedPod.spec.nodeName,
    appPodNode: appPod.spec.nodeName, sdkPodNode: sdkPod.spec.nodeName,
    sdkPodUIDVerified: true, applicationRecovered: true, sdkApplicationRecovered: true,
    traceSpans: event.trace.spans.length, sdkTraceSpans: event.sdkTrace.spans.length,
    kubernetesOOM: true
  };
} finally {
  if (!restored) await restore();
}
writeFileSync(resultPath, JSON.stringify(result, null, 2) + '\n');
console.log(JSON.stringify(result, null, 2));
