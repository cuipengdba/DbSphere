import { execFile } from 'node:child_process';
import { readFile, writeFile } from 'node:fs/promises';
import { setTimeout as sleep } from 'node:timers/promises';
import { promisify } from 'node:util';

const run = promisify(execFile);
const config = JSON.parse(await readFile(process.argv[2], 'utf8'));
const phase = process.argv[3] ?? 'load';
const site = new URL(config.site);
const assetCount = config.assetCount ?? 100;
const interval = config.intervalSeconds ?? 30;
const duration = config.durationSeconds ?? 8 * 3600;
const database = config.storeDatabase ?? 'postgres';
const monitor = config.monitorUser ?? 'dbsphere_collector';
const adminPassword = process.env.DBSPHERE_LOAD_ADMIN_PASSWORD;
const monitorPassword = process.env.DBSPHERE_LOAD_MONITOR_PASSWORD;
if (!adminPassword || !monitorPassword || !config.targets?.length || !config.storeContainer || !config.report ||
    !Number.isInteger(assetCount) || assetCount < 1 || assetCount > 1000 ||
    !Number.isInteger(interval) || interval < 10 || interval > 3600 ||
    !Number.isInteger(duration) || duration < 2 * interval || duration > 48 * 3600 ||
    !/^[a-z_][a-z_0-9]{0,62}$/.test(monitor) || !['load','failover'].includes(phase)) {
  throw new Error('Check the load-test configuration and the two password variables in the acceptance guide');
}

async function sql(container, db, statement) {
  const { stdout } = await run('docker', ['exec', container, 'psql', '-XAtq', '-U', 'postgres',
    '--dbname=' + db, '-v', 'ON_ERROR_STOP=1', '-c', statement], { timeout: 120_000, maxBuffer: 4_000_000 });
  return JSON.parse(stdout);
}

const storeSQL = statement => sql(config.storeContainer, database, statement);
async function authenticate() {
  const login = await fetch(new URL('/api/login', site), {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ username: config.adminUser ?? 'admin', password: adminPassword }),
    signal: AbortSignal.timeout(20_000),
  });
  if (!login.ok) throw new Error('Load-test administrator login returned ' + login.status);
  const { csrf } = await login.json();
  return { csrf, cookie: login.headers.getSetCookie().map(value => value.split(';')[0]).join('; ') };
}
let session = await authenticate();
async function api(path, body) {
  const response = await fetch(new URL(path, site), {
    method: body === undefined ? 'GET' : 'POST',
    headers: { Cookie: session.cookie, 'X-CSRF-Token': session.csrf, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(20_000),
  });
  if (!response.ok) throw new Error(path + ' returned ' + response.status);
  return response.json();
}

const identifiers = ids => {
  if (!Array.isArray(ids) || !ids.length || ids.some(id => !/^[0-9a-f-]{36}$/.test(id))) throw new Error('Invalid load-test asset IDs');
  return 'ARRAY[' + ids.map(id => "'" + id + "'").join(',') + ']::uuid[]';
};

if (phase === 'failover') {
  const ids = identifiers(JSON.parse(await readFile(config.report + '.assets.json','utf8')));
  const start = new Date(await storeSQL('SELECT to_jsonb(now())')).toISOString();
  let result;
  for (let tries = 0; tries <= 45; tries++) {
    result = await storeSQL(`SELECT jsonb_build_object('fresh',count(*) FILTER (WHERE last_success_at>='${start}'),
      'replicas',coalesce(jsonb_agg(DISTINCT last_replica_id) FILTER (WHERE last_success_at>='${start}'),'[]'::jsonb),
      'duplicateSlots',(SELECT count(*)-count(DISTINCT (asset_id,metrics_json#>>'{collector,scheduledFor}')) FROM samples
        WHERE asset_id=ANY(${ids}) AND status='ok' AND collected_at>='${start}' AND metrics_json#>>'{collector,scheduledFor}' IS NOT NULL))
      FROM collection_jobs WHERE asset_id=ANY(${ids})`);
    if (result.fresh === assetCount && result.replicas.length === 1 && result.replicas[0] && result.duplicateSlots === 0) break;
    await sleep(2000);
  }
  const pass = result.fresh === assetCount && result.replicas.length === 1 && Boolean(result.replicas[0]) && result.duplicateSlots === 0;
  const proof = { start, assetCount, ...result, pass };
  await writeFile(config.report + '.failover.json', JSON.stringify(proof, null, 2) + '\n');
  console.log(JSON.stringify(proof, null, 2));
  if (!pass) process.exitCode = 1;
  process.exit();
}

const marker = config.marker ?? 'collector-load-' + Date.now();
if (!/^collector-load-\d{13}$/.test(marker)) throw new Error('Invalid load-test batch marker');
const prepared = await storeSQL("SELECT coalesce(jsonb_object_agg(name,id),'{}') FROM assets WHERE name LIKE '" + marker + "-%'");
const assets = [];
async function register(name, target) {
  let id = prepared[name];
  if (!id) {
    const password = target.passwordEnv ? process.env[target.passwordEnv] : target.runtime === 'etcd' ? '' : monitorPassword;
    if (password === undefined) throw new Error('Missing fixture password variable ' + target.passwordEnv);
    const { asset } = await api('/api/assets', {
      name, host: target.host, port: target.port ?? 5432, database: target.database ?? 'postgres',
      username: target.username ?? monitor, password, sslMode: 'disable', runtime: target.runtime ?? 'managed',
    });
    id = asset.id;
  }
  if (!/^[0-9a-f-]{36}$/.test(id)) throw new Error('Invalid asset ID from API');
  return id;
}
for (let number = 0; number < assetCount; number++) {
  assets.push(await register(marker + '-' + number, config.targets[number % config.targets.length]));
}
const faults = [];
for (const [index, target] of (config.failureTargets ?? []).entries()) {
  faults.push(await register(marker + '-fault-' + index, target));
}
await writeFile(config.report + '.assets.json', JSON.stringify(assets, null, 2));
const assetIDs = identifiers(assets);
const targetCounters = async () => Promise.all(config.targets.filter(target => target.container).map(async target => ({
  target: target.host,
  ...await sql(target.container, target.database ?? 'postgres', "SELECT jsonb_build_object(" +
    "'version',current_setting('server_version'),'calls',coalesce(sum(calls),0),'executionMs',coalesce(sum(total_exec_time),0)) " +
    "FROM pg_stat_statements WHERE userid=(SELECT oid FROM pg_roles WHERE rolname='" + monitor + "')"),
})));
const storageBytes = () => storeSQL("SELECT to_jsonb(pg_database_size(current_database()))");
for (let tries = 0; ; tries++) {
  const sampled = await storeSQL("SELECT to_jsonb(count(*)) FROM collection_jobs WHERE last_success_at IS NOT NULL AND asset_id=ANY(" + assetIDs + ')');
  if (sampled === assetCount) break;
  if (tries === 90) throw new Error('Some healthy assets never completed warmup: ' + sampled + '/' + assetCount);
  await sleep(2000);
}
const before = { targetSQL: await targetCounters(), storageBytes: await storageBytes() };
const window = await storeSQL("SELECT jsonb_build_object('from',now(),'to',now()+make_interval(secs=>" + duration +
  "),'due',(SELECT jsonb_object_agg(asset_id,date_bin(make_interval(secs=>" + interval + "),now(),due_at)+" +
  "make_interval(secs=>" + interval + ")) FROM collection_jobs WHERE asset_id=ANY(" + assetIDs + ')))');
await writeFile(config.report + '.window.json', JSON.stringify({ ...window, marker, before }, null, 2));
console.log('Warmup complete, measuring ' + assetCount + ' assets from ' + window.from + ' through ' + window.to);
await sleep(duration * 1000 + interval * 1000);
const from = new Date(window.from).toISOString();
const to = new Date(window.to).toISOString();
const expected = Object.values(window.due).reduce((total, due) =>
  total + Math.max(0, 1 + Math.floor((Date.parse(to) - Date.parse(due)) / (interval * 1000))), 0);
const observed = await storeSQL([
  'WITH seen AS (',
  "SELECT asset_id,status,metrics_json->'collector' AS c FROM samples WHERE asset_id=ANY(" + assetIDs + ")",
  "AND collected_at>'" + from + "' AND collected_at<='" + to + "'::timestamptz+make_interval(secs=>" + interval + ')',
  '), scheduled AS (',
  "SELECT asset_id,status,(c->>'scheduledFor')::timestamptz AS due,(c->>'queueMs')::bigint AS queue,",
  "(c->>'queryMs')::bigint AS query,(c->>'replicaId') AS replica FROM seen",
  "WHERE c?'scheduledFor' AND (c->>'scheduledFor')::timestamptz>'" + from + "' AND (c->>'scheduledFor')::timestamptz<='" + to + "'",
  ") SELECT jsonb_build_object('samples',count(*),'ok',count(DISTINCT (asset_id,due)) FILTER(WHERE status='ok'),",
  "'duplicates',count(*)-count(DISTINCT (asset_id,due)),'replicas',array_agg(DISTINCT replica),'timed',count(queue),",
  "'queueP95Ms',percentile_disc(0.95) WITHIN GROUP (ORDER BY queue),",
  "'queryP95Ms',percentile_disc(0.95) WITHIN GROUP (ORDER BY query)) FROM scheduled",
].join('\n'));
const coverage = 100 * observed.ok / expected;
const isolation = faults.length ? await storeSQL(`SELECT jsonb_build_object('isolated',count(*) FILTER (WHERE isolated),
  'lastTargetErrors',count(*) FILTER (WHERE last_outcome='target_error')) FROM collection_jobs WHERE asset_id=ANY(${identifiers(faults)})`) : null;
const engineCoverage = await storeSQL(`SELECT jsonb_object_agg(engine,counts) FROM (
  SELECT coalesce(j.last_engine,a.runtime) AS engine,jsonb_build_object('targets',count(*),'permissionsChecked',count(j.last_permission_gaps),
  'limited',count(*) FILTER (WHERE j.last_permission_gaps>0),'p95WriteMs',percentile_disc(0.95) WITHIN GROUP (ORDER BY j.last_store_ms)) AS counts
  FROM assets a JOIN collection_jobs j ON j.asset_id=a.id WHERE a.id=ANY(${assetIDs}) GROUP BY coalesce(j.last_engine,a.runtime)) engines`);
const pass = coverage >= 99 && observed.duplicates === 0 && observed.timed > 0 && observed.queueP95Ms <= interval * 1000 &&
  (!isolation || isolation.isolated === faults.length && isolation.lastTargetErrors === faults.length) &&
  (!config.mixed || ['postgres','mysql','redis','etcd'].every(engine => engineCoverage[engine]?.targets > 0 &&
    engineCoverage[engine].permissionsChecked > 0 && engineCoverage[engine].p95WriteMs != null) &&
    engineCoverage.mysql.limited > 0 && observed.replicas.length === 2);
const report = { marker, assetCount, interval, duration, from, to, expected, observed, coverage, engineCoverage, isolation, pass };
await writeFile(config.report, JSON.stringify(report, null, 2) + '\n');
const after = { targetSQL: await targetCounters(), storageBytes: await storageBytes() };
const timings = { coverage: [], hourlySeries: [] };
session = await authenticate();
for (let index = 0; index < 5; index++) {
  for (const [metric, path] of [
    ['coverage', '/api/admin/collector/health'],
    ['hourlySeries', '/api/assets/' + assets[index % assets.length] + '/series?' + new URLSearchParams({
      from: new Date(Date.parse(to) - 30 * 86400_000).toISOString(), to, bucketHours: '1',
    })],
  ]) {
    const start = performance.now();
    await api(path);
    timings[metric].push(Number((performance.now() - start).toFixed(1)));
  }
}
Object.assign(report, {
  targetSQL: after.targetSQL.map((item, i) => ({ target: item.target, version: item.version,
    calls: item.calls - before.targetSQL[i].calls, executionMs: item.executionMs - before.targetSQL[i].executionMs })),
  storeBytesBefore: before.storageBytes, storeBytesAfter: after.storageBytes, apiMs: timings,
});
await writeFile(config.report, JSON.stringify(report, null, 2) + '\n');
console.log(JSON.stringify(report, null, 2));
if (!pass) process.exitCode = 1;
