#!/usr/bin/env node

import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { setTimeout as delay } from 'node:timers/promises';

const [baseURL, credentialsPath, kubeconfig, cluster] = process.argv.slice(2);
assert.ok(baseURL && credentialsPath && kubeconfig && cluster, 'BASE_URL CREDENTIALS_PATH KUBECONFIG CLUSTER are required');
const credentials = JSON.parse(readFileSync(credentialsPath, 'utf8'));
const namespace = 'dbsphere-matrix';
const service = 'dbsphere-route-check';
const kubectl = (args, input) => execFileSync('kubectl',
  ['--kubeconfig', kubeconfig, '-n', namespace, '--request-timeout=10s', ...args], { input, encoding: 'utf8', timeout: 15000 });
let cookie = '', csrf = '';
async function call(path, method = 'GET', payload) {
  const response = await fetch(baseURL + path, {
    method,
    headers: { ...(payload ? { 'Content-Type': 'application/json' } : {}), ...(cookie ? { Cookie: cookie } : {}),
      ...(method !== 'GET' && csrf ? { 'X-CSRF-Token': csrf } : {}) },
    body: payload ? JSON.stringify(payload) : undefined,
    signal: AbortSignal.timeout(25000)
  });
  const body = await response.json();
  assert.ok(response.ok, `${method} ${path} returned HTTP ${response.status}: ${body.error || ''}`);
  return { body, response };
}

function setTargetPort(targetPort) {
  kubectl(['apply', '-f', '-'], JSON.stringify({
    apiVersion: 'v1', kind: 'Service', metadata: { name: service },
    spec: { selector: { app: 'pg18' }, ports: [{ port: 5432, targetPort, protocol: 'TCP' }] }
  }));
}

const ready = (await call('/readyz')).body;
assert.equal(ready.storage, 'postgresql18');
const login = await call('/api/login', 'POST', { password: credentials.adminPassword });
cookie = login.response.headers.get('set-cookie')?.split(';')[0];
csrf = login.body.csrf;
assert.ok(cookie && csrf, 'authenticated test session required');
const { asset } = (await call('/api/assets', 'POST', {
  name: 'Kubernetes Service routing acceptance', host: 'pg18', port: 5432, database: 'postgres',
  username: 'dbsphere_monitor', password: credentials.monitorPassword, sslMode: 'disable', runtime: 'k8s',
  k8sCluster: cluster, namespace, workload: 'pg18'
})).body;
const prefix = `/api/assets/${asset.id}`;
const entry = `${service}.${namespace}.svc.cluster.local:5432`;
await call(prefix + '/routing', 'PATCH', { kind: 'k8s_service', entry, expectedRole: 'primary', matchDirect: true });
try {
  async function observe(status, targetPort) {
    let sample;
    for (let i = 0; i < 12; i++) {
      const { result } = (await call(prefix + '/check', 'POST')).body;
      sample = (await call(prefix + '/samples/' + result.sampleId)).body.sample;
      const route = sample.metrics.routing;
      if (route?.status === status && route.service?.portMapping?.targetPort === targetPort &&
          Object.values(route.service.slices).some(slice => slice.ports.some(port => port.port === targetPort))) {
        assert.equal(sample.status, 'ok', 'direct PG and Kubernetes object collection should remain healthy');
        assert.equal(route.service.podReady, true, 'Pod must remain ready');
        assert.ok(route.service.podUid && route.service.uid, 'Service and Pod identities are required');
        return sample;
      }
      await delay(350);
    }
    assert.fail(`expected ${status} at targetPort ${targetPort}: ${JSON.stringify(sample?.metrics?.routing)}`);
  }
  setTargetPort(5432);
  const baseline = await observe('ok', 5432);
  setTargetPort(6543);
  const fault = await observe('ready_pod_entry_unreachable', 6543);
  assert.equal(fault.metrics.routing.service.podUid, baseline.metrics.routing.service.podUid);
  assert.equal(fault.metrics.routing.service.uid, baseline.metrics.routing.service.uid);
  assert.equal(fault.metrics.routing.probe.status, 'unreachable');
  const { events } = (await call('/api/events')).body;
  const event = events.find(row => row.assetId === asset.id && row.status === 'open' && row.kind === 'alert_incident');
  assert.ok(event, 'Service fault must open an incident');
  const opened = (await call('/api/events/' + event.id)).body;
  assert.ok(opened.analysis.candidates.some(item => item.code === 'service_entry_unreachable' &&
    item.detail.service.podUid === fault.metrics.routing.service.podUid), 'incident must link Service and ready Pod');
  const trigger = opened.event.evidence.find(item => item.kind === 'alert_firing' && item.payload.metric === 'routing.entry_fault');
  assert.ok(trigger?.payload.sampleId, 'incident must link the raw triggering sample');
  const source = (await call(prefix + '/samples/' + trigger.payload.sampleId)).body.sample;
  if (source.metrics.routing.status === 'ready_pod_entry_unreachable') {
    assert.ok(opened.analysis.graph.edges.some(edge => edge.relation === 'selects_pod'),
      'unreachable routing graph should show the configured selector');
  }
  setTargetPort(5432);
  const recovered = await observe('ok', 5432);
  const resolved = (await call('/api/events/' + event.id)).body.event;
  assert.equal(resolved.status, 'resolved', 'original Service event must recover');
  const recovery = resolved.evidence.find(item => item.kind === 'alert_recovered' && item.payload.metric === 'routing.entry_fault');
  assert.ok(recovery?.payload.sampleId, 'recovery must link its raw sample');
  console.log(JSON.stringify({ eventId: event.id, assetId: asset.id, entry, podUid: fault.metrics.routing.service.podUid,
    serviceUid: fault.metrics.routing.service.uid, baseline: baseline.id, fault: fault.id, recovered: recovered.id,
    trigger: trigger.payload.sampleId, recovery: recovery.payload.sampleId }, null, 2));
} finally {
  kubectl(['delete', 'service', service, '--ignore-not-found']);
}
