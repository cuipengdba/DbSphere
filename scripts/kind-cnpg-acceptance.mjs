#!/usr/bin/env node
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync } from 'node:fs';

const [base, path, config, context, resultPath] = process.argv.slice(2);
assert.ok(base && path && config && context, 'usage: kind-cnpg-acceptance.mjs BASE CREDENTIALS KUBECONFIG CONTEXT');
const namespace = 'dbsphere-matrix', name = 'cnpg-drill', recoveryName = 'cnpg-restored', marker = 'dbsphere-cnpg-proof';
const credentials = JSON.parse(readFileSync(path, 'utf8'));
const run = (args, input) => execFileSync('kubectl', ['--kubeconfig', config, '--request-timeout=165s', '-n', namespace, ...args],
  { input, encoding:'utf8', timeout:185000 }).trim();
const get = (kind, id) => JSON.parse(run(['get',kind,id,'-o','json']));
const apply = resource => run(['apply','-f','-'], JSON.stringify(resource));
const waitReady = cluster => run(['wait','--for=condition=Ready','cluster/'+cluster,'--timeout=160s']);
const cluster = (id, instances) => ({apiVersion:'postgresql.cnpg.io/v1',kind:'Cluster',metadata:{name:id,namespace},spec:{
  imageName:'ghcr.io/cloudnative-pg/postgresql:18.7-system-trixie',instances,failoverDelay:20,
  storage:{size:'1Gi'},managed:{roles:[{name:'dbsphere_monitor',ensure:'present',login:true,
    inRoles:['pg_monitor'],passwordSecret:{name:'cnpg-drill-monitor'}}]}}});
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
async function until(probe, label, attempts = 30, wait = 1000) {
  for (let i=0;i<attempts;i++) {
    const value = await probe();
    if (value) return value;
    await delay(wait);
  }
  throw Error(label);
}

let cookie = '', csrf = '';
async function request(route, method, value) {
  const response = await fetch(base.replace(/\/$/,'')+route, {method, signal:AbortSignal.timeout(40000),
    headers:{Connection:'close','Content-Type':'application/json',...(cookie ? {Cookie:cookie} : {}),...(csrf ? {'X-CSRF-Token':csrf} : {})},
    body:value ? JSON.stringify(value) : undefined});
  const payload = await response.json();
  if (route === '/api/login' && response.ok) cookie = response.headers.get('set-cookie').split(';')[0];
  assert.ok(response.ok, `${method} ${route}: HTTP ${response.status}`);
  return payload;
}
const read = route => request(route,'GET');
const write = (route, body) => request(route,'POST',body);
const database = (pod, sql) => run(['exec','-i',pod,'-c','postgres','--','psql','-U','postgres','-d','postgres','-Atq','-v','ON_ERROR_STOP=1'],sql);
function canRead(resource) {
  try { return run(['auth','can-i','get',resource,'--as=system:serviceaccount:'+namespace+':dbsphere-reader']) === 'yes'; }
  catch (error) { if (String(error.stdout).trim() === 'no') return false; throw error; }
}

assert.ok(canRead('clusters.postgresql.cnpg.io'), 'DbSphere cannot read Cluster resources');
assert.equal(canRead('secrets'),false);
apply({apiVersion:'v1',kind:'Secret',type:'kubernetes.io/basic-auth',metadata:{name:'cnpg-drill-monitor',namespace},
  stringData:{username:'dbsphere_monitor',password:credentials.monitorPassword}});
apply(cluster(name,2));
waitReady(name);
const original = get('cluster',name);
const primary = original.status.currentPrimary;
assert.equal(original.status.readyInstances,2);
const podList = () => JSON.parse(run(['get','pods','-l','cnpg.io/cluster='+name,'-o','json'])).items;
const replica = podList().find(item => item.metadata.name !== primary && item.status.phase === 'Running');
assert.ok(replica?.status.podIP, 'CloudNativePG did not produce a running replica');

csrf = (await write('/api/login',{password:credentials.adminPassword})).csrf;
const input = (title, target, host, pod) => ({name:title,host,port:5432,database:'postgres',username:'dbsphere_monitor',
  password:credentials.monitorPassword,sslMode:'disable',namespace,k8sCluster:context,runtime:'k8s',clusterName:target,workload:target,...(pod ? {pod} : {})});
const previewPath = '/api/assets/import/cloudnativepg?'+new URLSearchParams({context,namespace});
const inventoryBefore = (await read('/api/assets')).assets.length;
const preview = await read(previewPath);
assert.ok(preview.clusters.some(item=>item.uid===original.metadata.uid && item.readWriteService===name+'-rw'));
assert.equal((await read('/api/assets')).assets.length,inventoryBefore,'CloudNativePG preview wrote assets');
const rwHost = `${name}-rw.${namespace}.svc`;
const {asset:rw} = await write('/api/assets',input('CNPG 主库入口',name,rwHost));
const {asset:ro} = await write('/api/assets',input('CNPG 只读副本',name,replica.status.podIP,replica.metadata.name));
const plan = await write('/api/assets/import/preview',{format:'yaml',discover:true,credentialRef:`asset:${rw.id}`,username:'dbsphere_monitor',
  content:JSON.stringify({assets:[{name:'CNPG 候选预览',host:rwHost,port:5432,database:'postgres',sslMode:'disable',
    runtime:'k8s',clusterName:name,workload:name,namespace,k8sCluster:context}]})});
assert.equal(plan.rows[0].discovery?.workloadIdentity?.operator?.uid,original.metadata.uid);
async function sample(asset) {
  const route='/api/assets/'+asset.id;
  const {result}=await write(route+'/check');
  return (await read(route+'/samples/'+result.sampleId)).sample;
}
async function observed(asset, role, instance) {
  return until(async () => {
    const data = await sample(asset), state = data.metrics.k8s?.cloudNativePG;
    return data.metrics.role===role && state?.databaseObservation?.status==='aligned' &&
      state.databaseObservation.pod===instance ? data : null;
  }, `Could not verify ${role} observation for ${instance}`, 16);
}
const before = await observed(rw,'primary',primary);
const standby = await observed(ro,'standby',replica.metadata.name);
assert.equal(before.metrics.k8s.cloudNativePG.uid,original.metadata.uid);
database(primary, `CREATE SCHEMA dbsphere_drill; CREATE TABLE dbsphere_drill.marker (value text); INSERT INTO dbsphere_drill.marker VALUES ('${marker}');`);
await until(() => database(replica.metadata.name,"SELECT to_regclass('dbsphere_drill.marker')") &&
  database(replica.metadata.name,'SELECT value FROM dbsphere_drill.marker')===marker,
  'The marker was not present on the replica before failover');

run(['delete','pod',primary,'--wait=false']);
const fault = await until(async () => {
  const data=await sample(rw);
  return data.metrics.collector.pgStatus==='availability' ? data : null;
}, 'The writer endpoint did not report the failover outage', 10);
const event = (await read('/api/events')).events.find(item => item.assetId===rw.id && item.status==='open' && item.eventGroup==='availability');
assert.ok(event?.id,'The operator failover outage did not open an availability event');
const newPrimary = await until(() => {
  const current = get('cluster',name).status.currentPrimary;
  return current && current!==primary ? current : null;
}, 'CloudNativePG did not promote a replacement primary', 50, 1800);
const after = await observed(rw,'primary',newPrimary);
assert.equal(database(newPrimary,'SELECT value FROM dbsphere_drill.marker'),marker);
const resolved = await until(async () => {
  const state = await read('/api/events/'+event.id);
  if (state.event.status === 'resolved') return state;
  await sample(rw);
  return null;
}, 'The original primary outage did not resolve', 15);
assert.ok(JSON.stringify(resolved).includes(fault.id),'Original outage sample is missing from the resolved event');

const dump=run(['exec',newPrimary,'-c','postgres','--','pg_dump','-U','postgres','-d','postgres','--schema=dbsphere_drill','--no-owner','--no-privileges']);
apply(cluster(recoveryName,1));
waitReady(recoveryName);
const restoredCluster=get('cluster',recoveryName);
const restoredPrimary=restoredCluster.status.currentPrimary;
database(restoredPrimary,dump+'\n');
assert.equal(database(restoredPrimary,'SELECT value FROM dbsphere_drill.marker'),marker);
const {asset:restored}=await write('/api/assets',input('CNPG 隔离恢复',recoveryName,`${recoveryName}-rw.${namespace}.svc`));
const verified = await observed(restored,'primary',restoredPrimary);
assert.equal(verified.metrics.k8s.cloudNativePG.uid,restoredCluster.metadata.uid);
assert.notEqual(verified.metrics.k8s.cloudNativePG.uid,original.metadata.uid);
assert.ok((await read(previewPath)).clusters.some(item=>item.uid===restoredCluster.metadata.uid));
const proof = {operator:'1.30.2',postgres:verified.metrics.version_num,asset:rw.id,event:event.id,
  originalPrimary:primary,replacementPrimary:newPrimary,initial:before.id,replica:standby.id,fault:fault.id,recovered:after.id,
  restoredCluster:restoredCluster.metadata.uid,restoredSample:verified.id,restoredMarker:marker};
const observation = sample => ({id:sample.id,collectedAt:sample.collectedAt,status:sample.status,
  metrics:{role:sample.metrics.role,collector:sample.metrics.collector,k8s:sample.metrics.k8s,version_num:sample.metrics.version_num}});
if (resultPath) writeFileSync(resultPath, JSON.stringify({proof,asset:rw,preview,
  samples:[before,standby,fault,after,verified].map(observation)},null,2));
console.log(JSON.stringify(proof,null,2));
