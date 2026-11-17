import { DatabaseSync } from 'node:sqlite';
import { existsSync, readFileSync } from 'node:fs';
import { join, resolve } from 'node:path';
import { isDeepStrictEqual } from 'node:util';
import { PgStore, ensureSamplePartition, ensureEventPartition } from '../src/pg-store.js';
import { decrypt, encrypt } from '../src/security.js';

const sourceDir = process.env.DBSPHERE_SQLITE_DATA_DIR;
const targetUrl = process.env.DBSPHERE_DATABASE_URL;
const targetKeyText = process.env.DBSPHERE_MASTER_KEY;
if (!sourceDir || !targetUrl || !targetKeyText) {
  throw new Error('Set DBSPHERE_SQLITE_DATA_DIR, DBSPHERE_DATABASE_URL and DBSPHERE_MASTER_KEY');
}
const sourcePath = join(resolve(sourceDir), 'dbsphere.sqlite');
if (!existsSync(sourcePath)) throw new Error('SQLite source database not found');

const oldKeyText = process.env.DBSPHERE_OLD_MASTER_KEY;
const oldKey = oldKeyText ? Buffer.from(oldKeyText, 'base64') : readFileSync(join(resolve(sourceDir), 'master.key'));
const targetKey = Buffer.from(targetKeyText, 'base64');
if (oldKey.length !== 32 || targetKey.length !== 32) throw new Error('Source and target master keys must be 32 bytes');

const source = new DatabaseSync(sourcePath, { readOnly: true });
const target = await PgStore.create(targetUrl);
const tables = ['settings', 'assets', 'samples', 'events', 'evidence', 'alert_rules', 'alert_states',
  'silences', 'notification_outbox', 'inspection_templates', 'inspection_schedules',
  'inspection_runs', 'slow_log_entries', 'sql_plan_snapshots'];
const available = new Set(source.prepare("SELECT name FROM sqlite_master WHERE type='table'").all().map(row => row.name));
const sourceRows = Object.fromEntries(tables.map(table => [table,
  available.has(table) ? source.prepare(`SELECT * FROM ${table}`).all() : []]));
const client = await target.pool.connect();
try {
  await client.query('BEGIN');
  await client.query('SELECT pg_advisory_xact_lock(1742026)');
  for (const table of tables.slice(1).filter(table => !['alert_rules', 'inspection_templates'].includes(table))) {
    const count = Number((await client.query(`SELECT count(*)::int AS count FROM ${table}`)).rows[0].count);
    if (count > 0) throw new Error(`Target ${table} is not empty; migration is append-unsafe`);
  }
  for (const row of sourceRows.settings) {
    await client.query(`INSERT INTO settings(key,value) VALUES($1,$2)
      ON CONFLICT(key) DO UPDATE SET value=excluded.value`, [row.key, row.value]);
  }
  for (const row of sourceRows.alert_rules) {
    await client.query(`INSERT INTO alert_rules(id,definition_json) VALUES($1,$2)
      ON CONFLICT(id) DO UPDATE SET definition_json=excluded.definition_json`, [row.id, row.definition_json]);
  }
  for (const row of sourceRows.inspection_templates) {
    await client.query(`INSERT INTO inspection_templates(id,definition_json,updated_at)
      VALUES($1,$2,$3) ON CONFLICT(id) DO UPDATE SET
      definition_json=excluded.definition_json,updated_at=excluded.updated_at`,
    [row.id, row.definition_json, row.updated_at]);
  }
  for (const row of sourceRows.assets) {
    const reencrypted = encrypt(targetKey, decrypt(oldKey, row.password_enc));
    const poolerUrl = row.pooler_url_enc ? encrypt(targetKey, decrypt(oldKey, row.pooler_url_enc)) : null;
    await client.query(`INSERT INTO assets
      (id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,linux_host,linux_exporter_port,pooler_url_enc,
       k8s_cluster,k8s_namespace,k8s_workload,k8s_pod,k8s_node,created_at,last_sample_at,
       last_status,last_error,version_num,role)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`, [
      row.id, row.name, row.host, row.port, row.database_name, row.username, reencrypted,
      row.ssl_mode, row.runtime, row.linux_host, row.linux_exporter_port ?? null, poolerUrl, row.k8s_cluster, row.k8s_namespace,
      row.k8s_workload, row.k8s_pod, row.k8s_node, row.created_at, row.last_sample_at,
      row.last_status, row.last_error, row.version_num, row.role
    ]);
  }
  for (const row of sourceRows.silences) {
    await client.query(`INSERT INTO silences(id,kind,asset_id,rule_id,starts_at,ends_at,reason,created_at,revoked_at)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, [row.id, row.kind, row.asset_id, row.rule_id,
      row.starts_at, row.ends_at, row.reason, row.created_at, row.revoked_at]);
  }
  for (const month of new Set(sourceRows.samples.map(row => row.collected_at.slice(0, 7)))) {
    await ensureSamplePartition(client, `${month}-01T00:00:00Z`);
  }
  for (const row of sourceRows.samples) {
    await client.query(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json,error)
      VALUES($1,$2,$3,$4,$5,$6)`, [row.id, row.asset_id, row.collected_at,
      row.status, row.metrics_json, row.error]);
  }
  for (const row of sourceRows.inspection_schedules) {
    await client.query(`INSERT INTO inspection_schedules
      (asset_id,template_id,interval_minutes,next_due_at,enabled) VALUES($1,$2,$3,$4,$5)`,
    [row.asset_id, row.template_id, row.interval_minutes, row.next_due_at, Boolean(row.enabled)]);
  }
  for (const row of sourceRows.inspection_runs) {
    await client.query(`INSERT INTO inspection_runs
      (id,asset_id,template_id,template_version,sample_id,retry_of,started_at,completed_at,
       summary_json,findings_json,status,error,event_ids_json)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
    [row.id, row.asset_id, row.template_id, row.template_version, row.sample_id,
      row.retry_of, row.started_at, row.completed_at, row.summary_json,
      row.findings_json, row.status, row.error, row.event_ids_json]);
  }
  for (const row of sourceRows.slow_log_entries) {
    await client.query(`INSERT INTO slow_log_entries
      (id,asset_id,source_key,observed_at,duration_ms,query_id,source_format,imported_at)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, [row.id, row.asset_id, row.source_key,
      row.observed_at, row.duration_ms, row.query_id, row.source_format, row.imported_at]);
  }
  for (const row of sourceRows.sql_plan_snapshots) {
    await client.query(`INSERT INTO sql_plan_snapshots
      (id,asset_id,query_id,observed_at,source,shape_hash,summary_json,imported_at)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, [row.id, row.asset_id, row.query_id,
      row.observed_at, row.source, row.shape_hash, row.summary_json, row.imported_at]);
  }
  for (const month of new Set(sourceRows.events.map(row => row.started_at.slice(0, 7)))) {
    await ensureEventPartition(client, `${month}-01T00:00:00Z`);
  }
  for (const row of sourceRows.events) {
    await client.query('INSERT INTO event_keys(id,asset_id,started_at) VALUES($1,$2,$3)',
      [row.id, row.asset_id, row.started_at]);
    await client.query(`INSERT INTO events(id,asset_id,kind,title,status,started_at,ended_at,event_group,acknowledged_at)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, [row.id, row.asset_id, row.kind,
      row.title, row.status, row.started_at, row.ended_at, row.event_group ?? 'telemetry', row.acknowledged_at ?? null]);
    if (row.status === 'open') await client.query(`INSERT INTO open_event_groups(asset_id,event_group,event_id)
      VALUES($1,$2,$3)`, [row.asset_id, row.event_group ?? 'telemetry', row.id]);
  }
  for (const row of sourceRows.evidence) {
    await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
      VALUES($1,$2,$3,$4,$5)`, [row.id, row.event_id, row.kind,
      row.observed_at, row.payload_json]);
  }
  for (const row of sourceRows.notification_outbox) {
    await client.query(`INSERT INTO notification_outbox
      (id,evidence_id,channel,status,attempts,next_attempt_at,delivered_at,last_error)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, [row.id, row.evidence_id, row.channel,
      row.status, row.attempts, row.next_attempt_at, row.delivered_at, row.last_error]);
  }
  for (const row of sourceRows.alert_states) {
    await client.query('INSERT INTO alert_states(asset_id,rule_id,state_json) VALUES($1,$2,$3)',
      [row.asset_id, row.rule_id, row.state_json]);
  }
  for (const table of tables.slice(1).filter(table => !['alert_rules', 'inspection_templates'].includes(table))) {
    const count = Number((await client.query(`SELECT count(*)::int AS count FROM ${table}`)).rows[0].count);
    if (count !== sourceRows[table].length) throw new Error(`Count mismatch in ${table}`);
  }
  const eventKeys = Number((await client.query('SELECT count(*)::int AS count FROM event_keys')).rows[0].count);
  const openGroups = Number((await client.query('SELECT count(*)::int AS count FROM open_event_groups')).rows[0].count);
  if (eventKeys !== sourceRows.events.length ||
    openGroups !== sourceRows.events.filter(row => row.status === 'open').length) {
    throw new Error('Event identity or open-group migration count mismatch');
  }
  for (const row of sourceRows.alert_rules) {
    const imported = (await client.query('SELECT definition_json FROM alert_rules WHERE id=$1', [row.id])).rows[0]?.definition_json;
    if (!isDeepStrictEqual(imported, JSON.parse(row.definition_json))) {
      throw new Error(`Alert rule mismatch: ${row.id}`);
    }
  }
  for (const row of sourceRows.inspection_templates) {
    const imported = (await client.query('SELECT definition_json FROM inspection_templates WHERE id=$1',
      [row.id])).rows[0]?.definition_json;
    if (!isDeepStrictEqual(imported, JSON.parse(row.definition_json))) {
      throw new Error(`Inspection template mismatch: ${row.id}`);
    }
  }
  await client.query('COMMIT');
  console.log(`Migrated SQLite to PostgreSQL 18: ${tables.map(table => `${table}=${sourceRows[table].length}`).join(', ')}`);
} catch (error) { await client.query('ROLLBACK'); throw error; }
finally { client.release(); source.close(); await target.close(); }
