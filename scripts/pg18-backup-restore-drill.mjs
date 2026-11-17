import assert from 'node:assert/strict';
import { randomUUID } from 'node:crypto';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve, sep } from 'node:path';
import { spawn } from 'node:child_process';
import pg from 'pg';
import { PgStore } from '../src/pg-store.js';
import { evaluateInspection } from '../src/inspections.js';

const connection = process.env.DBSPHERE_DATABASE_URL;
if (!connection) throw new Error('Set DBSPHERE_DATABASE_URL for the PostgreSQL 18 drill');
const root = resolve(process.env.DBSPHERE_TEST_TMP_DIR || process.env.RUNNER_TEMP || tmpdir());
await mkdir(root, { recursive: true });
const directory = await mkdtemp(join(root, 'dbsphere-pg18-drill-'));
const suffix = randomUUID().replaceAll('-', '').slice(0, 12);
const sourceName = `dbsphere_drill_source_${suffix}`;
const restoreName = `dbsphere_drill_restore_${suffix}`;
const url = new URL(connection);
const admin = new pg.Client({ connectionString: connection });
let sourceCreated = false;
let restoreCreated = false;
let store;

function databaseUrl(name) {
  const copy = new URL(url);
  copy.pathname = `/${name}`;
  return copy.toString();
}

function docker(args, input = null) {
  return new Promise((done, fail) => {
    const child = spawn('docker', args, { env: { ...process.env, PGPASSWORD: decodeURIComponent(url.password) },
      stdio: ['pipe', 'pipe', 'pipe'] });
    const stdout = [];
    const stderr = [];
    let size = 0;
    child.stdout.on('data', chunk => {
      size += chunk.length;
      if (size > 20 * 1024 * 1024) child.kill();
      else stdout.push(chunk);
    });
    child.stderr.on('data', chunk => stderr.push(chunk));
    child.on('error', fail);
    child.on('close', code => code === 0 ? done(Buffer.concat(stdout))
      : fail(new Error(`docker ${args[0]} failed: ${Buffer.concat(stderr).toString('utf8').slice(0, 500)}`)));
    child.stdin.on('error', () => {});
    child.stdin.end(input ?? undefined);
  });
}

try {
  await admin.connect();
  await admin.query(`CREATE DATABASE ${sourceName}`);
  sourceCreated = true;
  store = await PgStore.create(databaseUrl(sourceName));
  const asset = await store.addAsset({ name: 'PG18 restore drill', host: 'localhost', port: 5432,
    database: 'postgres', username: 'monitor', sslMode: 'disable', runtime: 'linux' }, 'encrypted-placeholder');
  await store.recordResult(asset.id, {}, 'connection refused');
  const event = (await store.listEvents())[0];
  assert.ok(event?.id);
  const template = await store.getInspectionTemplate('pg_daily');
  const [sample] = await store.listSamples(asset.id, 1);
  const result = evaluateInspection(template, asset, sample, null);
  const runId = randomUUID();
  await store.saveInspectionRun({ id: runId, assetId: asset.id, templateId: template.id,
    templateVersion: template.version, sampleId: sample.id, retryOf: null,
    startedAt: sample.collectedAt, completedAt: sample.collectedAt,
    summary: result.summary, findings: result.findings, eventIds: [event.id] });
  await store.close();
  store = null;
  const containers = (await docker(['ps', '--filter', 'publish=15434', '--format', '{{.ID}}'])).toString('utf8').trim().split(/\s+/).filter(Boolean);
  assert.equal(containers.length, 1, 'Expected one PostgreSQL 18 test container publishing port 15434');
  const container = containers[0];
  const dump = await docker(['exec', '-e', 'PGPASSWORD', container, 'pg_dump', '-h', '127.0.0.1',
    '-U', decodeURIComponent(url.username), '-Fc', '-d', sourceName]);
  const backupFile = join(directory, 'pg18-monitor-store.dump');
  await writeFile(backupFile, dump, { mode: 0o600 });
  assert.ok((await readFile(backupFile)).length > 1000);
  await admin.query(`CREATE DATABASE ${restoreName}`);
  restoreCreated = true;
  await docker(['exec', '-i', '-e', 'PGPASSWORD', container, 'pg_restore', '-h', '127.0.0.1',
    '-U', decodeURIComponent(url.username), '--no-owner', '--no-acl', '-d', restoreName], await readFile(backupFile));
  store = await PgStore.create(databaseUrl(restoreName));
  assert.equal((await store.getAsset(asset.id)).name, asset.name);
  assert.equal((await store.listSamples(asset.id, 10)).length, 1);
  assert.ok((await store.getEvent(event.id)).evidence.length >= 1);
  assert.equal((await store.getInspectionRun(runId)).eventIds[0], event.id);
  console.log('PG18 monitoring store backup, isolated restore, event evidence and inspection history passed');
} finally {
  if (store) await store.close();
  if (sourceCreated) await admin.query(`DROP DATABASE ${sourceName} WITH (FORCE)`);
  if (restoreCreated) await admin.query(`DROP DATABASE ${restoreName} WITH (FORCE)`);
  await admin.end().catch(() => {});
  if (!resolve(directory).startsWith(root + sep)) throw new Error('Refusing to remove drill files outside test root');
  await rm(directory, { recursive: true, force: true });
}
