#!/usr/bin/env node

// Read-only Kubernetes and incident acceptance against an already deployed Go server.
// The only application write is a fresh monitoring sample of the recovered target.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';

const [baseURL, passwordPath, kubeconfigPath, namespace, eventId, resultPath,
  goDeployment = 'dbsphere-go', storeDeployment = 'dbsphere-go-store'] = process.argv.slice(2);
if (!baseURL || !passwordPath || !kubeconfigPath || !namespace || !eventId || !resultPath) {
  console.error('usage: node scripts/kind-live-acceptance.mjs BASE_URL PASSWORD_FILE KUBECONFIG NAMESPACE EVENT_ID RESULT_FILE [GO_DEPLOYMENT STORE_DEPLOYMENT]');
  process.exit(2);
}
const base = baseURL.replace(/\/$/, '');
const password = readFileSync(passwordPath, 'utf8').trim();
assert.ok(password, 'admin password file is empty');
let cookie = '';
let csrf = '';

function kubectl(args) {
  return execFileSync('kubectl', ['--kubeconfig', kubeconfigPath, '--request-timeout=15s', ...args],
    { encoding: 'utf8', timeout: 20000 }).trim();
}
function canI(verb, resource, scope = namespace) {
  const args = ['auth', 'can-i', verb, resource,
    `--as=system:serviceaccount:${namespace}:dbsphere-reader`];
  if (!['nodes', 'persistentvolumes'].includes(resource)) args.push('-n', scope);
  try { return kubectl(args) === 'yes'; }
  catch (error) {
    if (String(error.stdout).trim() === 'no') return false;
    throw error;
  }
}
async function api(path, method = 'GET') {
  const response = await fetch(base + path, {
    method, headers: {
      ...(cookie ? { Cookie: cookie } : {}),
      ...(method !== 'GET' && csrf ? { 'X-CSRF-Token': csrf } : {})
    }, signal: AbortSignal.timeout(45000)
  });
  const body = await response.json();
  assert.ok(response.ok, `${method} ${path}: ${response.status} ${JSON.stringify(body)}`);
  return body;
}

assert.equal(kubectl(['get', '--raw', '/readyz']), 'ok');
const nodes = JSON.parse(kubectl(['get', 'nodes', '-o', 'json'])).items;
assert.ok(nodes.some(node => node.status.conditions.some(c => c.type === 'Ready' && c.status === 'True')));
const deployments = JSON.parse(kubectl(['-n', namespace, 'get', 'deploy', '-o', 'json'])).items;
for (const name of [goDeployment, storeDeployment]) {
  const deployment = deployments.find(row => row.metadata.name === name);
  assert.ok(deployment?.status.readyReplicas >= 1, `${name} is not ready`);
}
for (const [verb, resource] of [['get', 'pods'], ['list', 'events'], ['get', 'nodes'],
  ['get', 'persistentvolumeclaims'], ['get', 'persistentvolumes']]) {
  assert.ok(canI(verb, resource), `collector cannot ${verb} ${resource}`);
}
for (const [verb, resource, scope] of [['get', 'secrets', namespace],
  ['delete', 'pods', namespace], ['list', 'pods', 'kube-system']]) {
  assert.equal(canI(verb, resource, scope), false,
    `collector can unexpectedly ${verb} ${resource} in ${scope}`);
}
const ready = await api('/readyz');
assert.equal(ready.storage, 'postgresql18');
const frontend = await fetch(base + '/', { signal: AbortSignal.timeout(10000) });
assert.equal(frontend.status, 200);
assert.match(await frontend.text(), /<html/i);
const login = await fetch(base + '/api/login', {
  method: 'POST', headers: { 'Content-Type': 'application/json' },
  body: JSON.stringify({ password }), signal: AbortSignal.timeout(10000)
});
assert.equal(login.status, 200);
cookie = login.headers.get('set-cookie')?.split(';')[0] || '';
csrf = (await login.json()).csrf;
assert.ok(cookie && csrf, 'admin cookie or CSRF token missing');
const ai = await api('/api/ai/status');
assert.equal(ai.enabled, false, 'no-model acceptance requires AI to be disabled');
const storage = (await api('/api/storage')).storage;
assert.equal(storage.mode, 'postgresql18');
const incident = await api(`/api/events/${eventId}`);
assert.equal(incident.event.id, eventId);
assert.equal(incident.event.status, 'resolved');
assert.ok(incident.event.evidence.length > 0);
assert.ok(incident.analysis.graph.nodes.length > 0);
assert.ok(incident.analysis.graph.edges.length > 0);
const oom = incident.analysis.candidates.find(row => /oom/i.test(row.code));
assert.ok(oom?.supports?.length, 'same-event OOM root-cause evidence missing');
assert.ok(incident.analysis.traceCoverage.spans >= 2);
assert.ok(incident.analysis.traceCoverage.databaseSpans >= 1);
assert.equal(incident.analysis.traceCoverage.unresolvedParentSpans, 0);
const assetId = incident.event.assetId;
const checked = (await api(`/api/assets/${assetId}/check`, 'POST')).result;
assert.equal(checked.status, 'ok', 'recovered PG target cannot be sampled');
const sample = (await api(`/api/assets/${assetId}/samples/${checked.sampleId}`)).sample;
assert.equal(sample.status, 'ok');
assert.equal(Math.floor(sample.metrics.version_num / 10000), 18);
assert.equal(sample.metrics.monitor_role, true);
assert.equal(sample.metrics.privileged_role, false);
assert.equal(sample.metrics.k8s?.status, 'ok');
assert.equal(sample.metrics.k8s?.pod?.phase, 'Running');
assert.ok(sample.metrics.k8s?.node?.name);
assert.ok(sample.metrics.k8s?.services?.length);
const pvcTarget = (await api('/api/assets')).assets.find(row =>
  row.runtime === 'k8s' && row.namespace === namespace && row.host === 'postgres');
assert.ok(pvcTarget?.id, 'dedicated PostgreSQL PVC fixture is missing');
const pvcCheck = (await api(`/api/assets/${pvcTarget.id}/check`, 'POST')).result;
assert.equal(pvcCheck.status, 'ok');
const pvcSample = (await api(`/api/assets/${pvcTarget.id}/samples/${pvcCheck.sampleId}`)).sample;
assert.ok(pvcSample.metrics.k8s?.eventsError == null);
assert.ok(pvcSample.metrics.k8s?.pvcs?.some(row => row.name === 'postgres-data'
  && row.phase === 'Bound' && row.volume?.phase === 'Bound'),
  'live PVC/PV association missing');
assert.ok(pvcSample.metrics.k8s?.resourceMetrics?.containers?.some(row => row.name === 'postgres'
  && row.memory), 'live Pod resource metrics missing');
assert.ok(pvcSample.metrics.k8s?.resourceMetrics?.node?.memory,
  'live Node resource metrics missing');
const reportResponse = await fetch(`${base}/api/events/${eventId}/report?format=md`,
  { headers: { Cookie: cookie }, signal: AbortSignal.timeout(20000) });
assert.equal(reportResponse.status, 200);
const report = await reportResponse.text();
assert.ok(report.includes(eventId) && report.includes(oom.code));
const result = {
  clusterNodesReady: nodes.filter(node => node.status.conditions.some(c =>
    c.type === 'Ready' && c.status === 'True')).length,
  namespace, goDeployment, storeDeployment, storage: ready.storage,
  sampleRetentionDays: storage.retentionDays,
  eventRetentionDays: storage.eventRetentionDays, aiEnabled: ai.enabled,
  eventId, eventStatus: incident.event.status, assetId, sampleId: checked.sampleId,
  pgMajor: 18, k8sPod: sample.metrics.k8s.pod.name,
  k8sNode: sample.metrics.k8s.node.name, evidence: incident.event.evidence.length,
  pvcSampleId: pvcCheck.sampleId, pvc: 'postgres-data',
  podNodeMetrics: true,
  graphNodes: incident.analysis.graph.nodes.length,
  graphEdges: incident.analysis.graph.edges.length,
  rootCauseCandidate: oom.code,
  traceSpans: incident.analysis.traceCoverage.spans,
  databaseSpans: incident.analysis.traceCoverage.databaseSpans,
  reportContainsEvidence: true, collectorReadOnly: true
};
writeFileSync(resultPath, `${JSON.stringify(result, null, 2)}\n`);
console.log(JSON.stringify(result, null, 2));
