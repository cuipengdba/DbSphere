#!/usr/bin/env node
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';

const [base, credentialsFile, config, context, namespace = 'dbsphere-matrix'] = process.argv.slice(2);
assert.ok(base && credentialsFile && config && context, 'usage: kind-kubernetes-identity.mjs BASE CREDENTIALS KUBECONFIG CONTEXT [NAMESPACE]');
const { adminPassword, monitorPassword } = JSON.parse(readFileSync(credentialsFile, 'utf8'));
const kubectl = process.env.DBSPHERE_TEST_KUBECTL || 'kubectl';
const run = (args, input) => execFileSync(kubectl, ['--kubeconfig', config, '--request-timeout=175s', '-n', namespace, ...args],
  { input, encoding: 'utf8', timeout: 180000 }).trim();
const get = (type, name) => JSON.parse(run(['get', type, name, '-o', 'json']));
const apply = resource => run(['apply', '-f', '-'], JSON.stringify(resource));
const name = 'uid-drill-pg', pvc = 'uid-drill-data', pod = name + '-0', slice = 'uid-drill-stale';
const volume = `${namespace}-uid-drill-static`;
const node = JSON.parse(run(['get','nodes','-o','json'])).items.find(item =>
  !Object.hasOwn(item.metadata.labels, 'node-role.kubernetes.io/control-plane') &&
  item.status.conditions.some(condition => condition.type === 'Ready' && condition.status === 'True'));
assert.ok(node, 'The identity drill needs a ready worker');
const hostname = node.metadata.labels['kubernetes.io/hostname'];
assert.ok(hostname, 'The selected worker has no hostname label');
const metadata = name => ({ name, namespace });
const wait = (object, condition = 'Ready') => run(['wait', `--for=condition=${condition}`, object, '--timeout=150s']);
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(check, label, attempts = 25) {
  for (let attempt = 0; attempt < attempts; attempt++) {
    const result = await check();
    if (result) return result;
    await sleep(1000);
  }
  throw Error(label);
}

let cookie = '', csrf = '';
async function api(path, method = 'GET', body) {
  const response = await fetch(base.replace(/\/$/, '') + path, {
    method, body: body ? JSON.stringify(body) : undefined, signal: AbortSignal.timeout(35000),
    headers: { 'content-type': 'application/json', ...(cookie ? { Cookie: cookie } : {}), ...(csrf ? { 'X-CSRF-Token': csrf } : {}) }
  });
  assert.ok(response.ok, `${method} ${path} returned ${response.status}`);
  if (path === '/api/login') cookie = response.headers.get('set-cookie').split(';')[0];
  return response.json();
}
csrf = (await api('/api/login', 'POST', { password: adminPassword })).csrf;
const startedAt = new Date(Date.now() - 2000).toISOString();
apply({apiVersion:'v1',kind:'PersistentVolumeClaim',metadata:metadata(pvc),spec:{storageClassName:'',volumeName:volume,
  accessModes:['ReadWriteOnce'],resources:{requests:{storage:'256Mi'}}}});
apply({apiVersion:'v1',kind:'Service',metadata:metadata(name),spec:{selector:{app:name},ports:[{port:5432,targetPort:5432}]}});
apply({apiVersion:'apps/v1',kind:'StatefulSet',metadata:metadata(name),spec:{replicas:1,serviceName:name,selector:{matchLabels:{app:name}},
  template:{metadata:{labels:{app:name}}, spec:{terminationGracePeriodSeconds:10,nodeSelector:{'kubernetes.io/hostname':hostname},
    containers:[{name:'postgres',image:'postgres:18-bookworm',ports:[{containerPort:5432}],
      env:[{name:'POSTGRES_PASSWORD',valueFrom:{secretKeyRef:{name:'dbsphere-matrix-credentials',key:'postgresPassword'}}},
        {name:'DBSPHERE_MONITOR_PASSWORD',valueFrom:{secretKeyRef:{name:'dbsphere-matrix-credentials',key:'monitorPassword'}}}],
      readinessProbe:{exec:{command:['pg_isready','-h','127.0.0.1','-U','postgres']},periodSeconds:2},
      volumeMounts:[{name:'pgdata',mountPath:'/var/lib/postgresql'},{name:'init',mountPath:'/docker-entrypoint-initdb.d',readOnly:true}]}],
    volumes:[{name:'pgdata',persistentVolumeClaim:{claimName:pvc}},{name:'init',configMap:{name:'dbsphere-monitor-init',defaultMode:0o755}}]}}}});
const originalPod = await until(() => {
  const raw = run(['get','pod',pod,'--ignore-not-found','-o','json']);
  return raw ? JSON.parse(raw) : null;
}, 'StatefulSet did not create a Pending Pod');
const claim = get('pvc',pvc);
assert.equal(claim.status.phase, 'Pending');
const { asset } = await api('/api/assets','POST',{name:'Kubernetes UID 验收',clusterName:'Kubernetes 对象演练',
  host:`${name}.${namespace}.svc.cluster.local`,port:5432,database:'postgres',username:'dbsphere_monitor',
  password:monitorPassword,sslMode:'disable',runtime:'k8s',k8sCluster:context,namespace,workload:name,pod});
const root = `/api/assets/${asset.id}`;
const sample = async () => {
  const { result } = await api(root+'/check','POST');
  return (await api(root+'/samples/'+result.sampleId)).sample;
};
const pending = await sample();
assert.equal(pending.metrics.k8s.pod.uid, originalPod.metadata.uid);
assert.equal(pending.metrics.k8s.workload.uid, get('statefulset',name).metadata.uid);
assert.equal(pending.metrics.k8s.pvcs[0].phase, 'Pending');
assert.equal(pending.metrics.k8s.pvcs[0].uid, claim.metadata.uid);
const event = (await api('/api/events')).events.find(item => item.assetId === asset.id && item.status === 'open' && item.eventGroup === 'availability');
assert.ok(event?.id, 'Initial unavailable PostgreSQL target did not open an event');
apply({apiVersion:'v1',kind:'PersistentVolume',metadata:{name:volume},spec:{storageClassName:'',capacity:{storage:'256Mi'},
  accessModes:['ReadWriteOnce'],persistentVolumeReclaimPolicy:'Retain',
  claimRef:{name:pvc,namespace,uid:claim.metadata.uid},
  nodeAffinity:{required:{nodeSelectorTerms:[{matchExpressions:[{key:'kubernetes.io/hostname',operator:'In',values:[hostname]}]}]}},
  hostPath:{path:`/var/local/${volume}`,type:'DirectoryOrCreate'}}});
wait('pod/'+pod);

function routes(reading) { return Object.values(reading.metrics.k8s.endpointRoutes?.slices || {}); }
const bound = await until(async () => {
  const reading = await sample();
  return reading.metrics.collector.pgStatus === 'ok' && routes(reading).some(item => item.endpoint.conditions.ready === true) ? reading : null;
}, 'The current EndpointSlice did not become ready', 12);
assert.equal(bound.metrics.k8s.pvcs[0].phase, 'Bound');
assert.equal(bound.metrics.k8s.pvcs[0].volume.claimUID, claim.metadata.uid);
assert.ok(bound.metrics.k8s.node.uid, 'Node UID was not verified');
assert.equal((await api(`/api/events/${event.id}`)).event.status, 'resolved');
const oldUID = bound.metrics.k8s.pod.uid;
run(['delete','pod',pod,'--wait=true','--timeout=90s']);
await until(() => {
  const raw = run(['get','pod',pod,'--ignore-not-found','-o','json']);
  return raw && JSON.parse(raw).metadata.uid !== oldUID;
}, 'StatefulSet did not replace the Pod at the same name');
wait('pod/'+pod);
const newUID = get('pod',pod).metadata.uid;
assert.notEqual(newUID, oldUID);
apply({apiVersion:'discovery.k8s.io/v1',kind:'EndpointSlice',metadata:{...metadata(slice),
  labels:{'kubernetes.io/service-name':name,'endpointslice.kubernetes.io/managed-by':'dbsphere-test'}},
  addressType:'IPv4',ports:[{port:5432,protocol:'TCP'}],endpoints:[{addresses:['192.0.2.10'],
    conditions:{ready:false,serving:false,terminating:true},targetRef:{kind:'Pod',name:pod,namespace,uid:oldUID}}]});
const replaced = await until(async () => {
  const reading = await sample();
  return reading.metrics.k8s.pod?.uid === newUID && routes(reading).some(item => item.endpoint.targetUID === newUID) ? reading : null;
}, 'EndpointSlice did not follow the replacement UID', 12);
assert.ok(replaced.metrics.k8s.endpointRoutes.identityMismatches >= 1);
assert.ok(routes(replaced).every(item => item.endpoint.targetUID === newUID));
assert.equal(replaced.metrics.k8s.pvcs[0].volume.claimUID, claim.metadata.uid);
const rules = get('role','dbsphere-reader').rules;
function replaceRules(next) { run(['patch','role','dbsphere-reader','--type=merge','-p',JSON.stringify({rules:next})]); }
let degraded;
try {
  replaceRules(rules.filter(rule => !rule.resources.includes('endpointslices')));
  degraded = await until(async () => {
    const reading = await sample();
    return reading.metrics.k8s.endpointRoutes?.status === 'unavailable' ? reading : null;
  }, 'Permission removal did not surface an EndpointSlice gap', 10);
  assert.equal(degraded.metrics.collector.pgStatus, 'ok');
  assert.equal(degraded.metrics.k8s.pod.uid, newUID);
} finally { replaceRules(rules); }
const to = new Date().toISOString();
const query = new URLSearchParams({from:startedAt,to,bucketMinutes:'1'});
const history = (await api(root+'/environment?'+query)).changes;
assert.equal(history.find(item => item.id === pending.id)?.pvcs?.[pvc], 'Pending');
assert.ok(history.some(item => item.uid === oldUID && item.pvcs?.[pvc] === 'Bound'), 'PVC binding history was lost');
assert.ok(history.some(item => item.uid === newUID && item.previousUID === oldUID), 'Previous Pod identity was lost');
assert.equal((await api(root+'/samples/'+pending.id)).sample.metrics.k8s.pod.uid, oldUID);
assert.equal((await api(`/api/events/${event.id}`)).event.status, 'resolved');
console.log(JSON.stringify({ asset:asset.id, namespace, pod, startedAt, to, oldUID, newUID, claimUID:claim.metadata.uid,
  event:event.id, pending:pending.id, bound:bound.id, replaced:replaced.id, denied:degraded.id }, null, 2));
