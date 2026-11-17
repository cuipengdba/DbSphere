import pg from 'pg';
import { randomUUID } from 'node:crypto';
import { oomCandidate } from './incidents.js';
import { publicAsset, publicSilence } from './store.js';
import { DEFAULT_RULES, readSignal, validateRuleUpdate } from './alert-rules.js';
import { nextAlertState } from './alert-state.js';
import { DEFAULT_INSPECTION_TEMPLATES, upgradeInspectionTemplate, validateInspectionTemplateUpdate } from './inspections.js';

const { Pool } = pg;

function timeMonth(value, table) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) throw new Error('Invalid sample partition timestamp');
  if (date.getUTCFullYear() < 1 || date.getUTCFullYear() > 9999) {
    throw new Error('Sample partition timestamp is outside supported years');
  }
  const start = new Date(Date.UTC(date.getUTCFullYear(), date.getUTCMonth(), 1));
  const end = new Date(Date.UTC(date.getUTCFullYear(), date.getUTCMonth() + 1, 1));
  const name = `${table}_${start.getUTCFullYear()}${String(start.getUTCMonth() + 1).padStart(2, '0')}`;
  return { name, start: start.toISOString(), end: end.toISOString() };
}

const sampleMonth = value => timeMonth(value, 'samples');
const eventMonth = value => timeMonth(value, 'events');

const inspectionRun = row => ({ id: row.id, assetId: row.asset_id, templateId: row.template_id,
  templateVersion: row.template_version, sampleId: row.sample_id, retryOf: row.retry_of,
  startedAt: row.started_at, completedAt: row.completed_at, status: row.status, error: row.error,
  eventIds: row.event_ids_json,
  summary: row.summary_json, findings: row.findings_json });

export async function ensureSamplePartition(client, timestamp) {
  const month = sampleMonth(timestamp);
  await client.query(`CREATE TABLE IF NOT EXISTS ${month.name} PARTITION OF samples
    FOR VALUES FROM ('${month.start}') TO ('${month.end}')`);
  return month.name;
}

export async function ensureEventPartition(client, timestamp) {
  const month = eventMonth(timestamp);
  await client.query(`CREATE TABLE IF NOT EXISTS ${month.name} PARTITION OF events
    FOR VALUES FROM ('${month.start}') TO ('${month.end}')`);
  return month.name;
}

const schema = `
  CREATE TABLE IF NOT EXISTS dbsphere_schema_migrations (
    version integer PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now()
  );
  CREATE TABLE IF NOT EXISTS settings (
    key text PRIMARY KEY, value text NOT NULL
  );
  CREATE TABLE IF NOT EXISTS assets (
    id uuid PRIMARY KEY, name text NOT NULL, host text NOT NULL, port integer NOT NULL,
    database_name text NOT NULL, username text NOT NULL, password_enc text NOT NULL,
    ssl_mode text NOT NULL, runtime text NOT NULL, linux_host text, linux_exporter_port integer, pooler_url_enc text, k8s_cluster text,
    k8s_namespace text, k8s_workload text, k8s_pod text, k8s_node text,
    created_at timestamptz NOT NULL, last_sample_at timestamptz,
    last_status text NOT NULL DEFAULT 'pending', last_error text, version_num integer, role text
  );
  ALTER TABLE assets ADD COLUMN IF NOT EXISTS linux_exporter_port integer;
  ALTER TABLE assets ADD COLUMN IF NOT EXISTS pooler_url_enc text;
  CREATE TABLE IF NOT EXISTS samples (
    id uuid NOT NULL, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    collected_at timestamptz NOT NULL, status text NOT NULL,
    metrics_json jsonb, error text, PRIMARY KEY(id,collected_at)
  ) PARTITION BY RANGE (collected_at);
  CREATE INDEX IF NOT EXISTS samples_asset_time ON samples(asset_id, collected_at DESC);
  CREATE TABLE IF NOT EXISTS event_keys (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    started_at timestamptz NOT NULL
  );
  CREATE TABLE IF NOT EXISTS events (
    id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    kind text NOT NULL, title text NOT NULL, status text NOT NULL,
    started_at timestamptz NOT NULL, ended_at timestamptz, event_group text, acknowledged_at timestamptz,
    PRIMARY KEY(id,started_at)
  ) PARTITION BY RANGE (started_at);
  ALTER TABLE events ADD COLUMN IF NOT EXISTS event_group text;
  ALTER TABLE events ADD COLUMN IF NOT EXISTS acknowledged_at timestamptz;
  UPDATE events SET event_group='telemetry' WHERE event_group IS NULL;
  CREATE INDEX IF NOT EXISTS events_asset_status ON events(asset_id, status);
  DROP INDEX IF EXISTS events_one_open_kind;
  DROP INDEX IF EXISTS events_one_open_group;
  CREATE TABLE IF NOT EXISTS open_event_groups (
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    event_group text NOT NULL, event_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
    PRIMARY KEY(asset_id,event_group)
  );
  CREATE TABLE IF NOT EXISTS evidence (
    id uuid PRIMARY KEY, event_id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
    kind text NOT NULL, observed_at timestamptz NOT NULL, payload_json jsonb NOT NULL
  );
  CREATE INDEX IF NOT EXISTS evidence_event_time ON evidence(event_id, observed_at);
  CREATE TABLE IF NOT EXISTS alert_rules (
    id text PRIMARY KEY, definition_json jsonb NOT NULL, updated_at timestamptz NOT NULL DEFAULT now()
  );
  CREATE TABLE IF NOT EXISTS alert_states (
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    rule_id text NOT NULL REFERENCES alert_rules(id), state_json jsonb NOT NULL,
    PRIMARY KEY(asset_id,rule_id)
  );
  CREATE TABLE IF NOT EXISTS silences (
    id uuid PRIMARY KEY, kind text NOT NULL, asset_id uuid REFERENCES assets(id) ON DELETE CASCADE,
    rule_id text REFERENCES alert_rules(id), starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL, reason text NOT NULL,
    created_at timestamptz NOT NULL, revoked_at timestamptz
  );
  CREATE INDEX IF NOT EXISTS silences_active ON silences(starts_at,ends_at);
  CREATE TABLE IF NOT EXISTS notification_outbox (
    id uuid PRIMARY KEY, evidence_id uuid NOT NULL REFERENCES evidence(id) ON DELETE CASCADE,
    channel text NOT NULL, status text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0, next_attempt_at timestamptz NOT NULL,
    delivered_at timestamptz, last_error text, UNIQUE(evidence_id,channel)
  );
  CREATE INDEX IF NOT EXISTS notification_outbox_due ON notification_outbox(status,next_attempt_at);
  CREATE TABLE IF NOT EXISTS inspection_templates (
    id text PRIMARY KEY, definition_json jsonb NOT NULL, updated_at timestamptz NOT NULL
  );
  CREATE TABLE IF NOT EXISTS inspection_schedules (
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    template_id text NOT NULL REFERENCES inspection_templates(id),
    interval_minutes integer NOT NULL, next_due_at timestamptz NOT NULL,
    enabled boolean NOT NULL DEFAULT true, PRIMARY KEY(asset_id,template_id)
  );
  CREATE INDEX IF NOT EXISTS inspection_schedules_due ON inspection_schedules(enabled,next_due_at);
  CREATE TABLE IF NOT EXISTS inspection_runs (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    template_id text NOT NULL REFERENCES inspection_templates(id), template_version integer NOT NULL,
    sample_id uuid, retry_of uuid, started_at timestamptz NOT NULL,
    completed_at timestamptz NOT NULL, summary_json jsonb NOT NULL, findings_json jsonb NOT NULL,
    status text NOT NULL DEFAULT 'completed', error text,
    event_ids_json jsonb NOT NULL DEFAULT '[]'::jsonb
  );
  ALTER TABLE inspection_runs ADD COLUMN IF NOT EXISTS status text NOT NULL DEFAULT 'completed';
  ALTER TABLE inspection_runs ADD COLUMN IF NOT EXISTS error text;
  ALTER TABLE inspection_runs ADD COLUMN IF NOT EXISTS event_ids_json jsonb NOT NULL DEFAULT '[]'::jsonb;
  CREATE INDEX IF NOT EXISTS inspection_runs_asset_time ON inspection_runs(asset_id,completed_at DESC);
  CREATE TABLE IF NOT EXISTS slow_log_entries (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    source_key text NOT NULL, observed_at timestamptz NOT NULL, duration_ms double precision NOT NULL,
    query_id text, source_format text NOT NULL, imported_at timestamptz NOT NULL,
    UNIQUE(asset_id,source_key)
  );
  CREATE INDEX IF NOT EXISTS slow_log_asset_time ON slow_log_entries(asset_id,observed_at DESC);
  CREATE TABLE IF NOT EXISTS sql_plan_snapshots (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    query_id text NOT NULL, observed_at timestamptz NOT NULL, source text NOT NULL,
    shape_hash text NOT NULL, summary_json jsonb NOT NULL, imported_at timestamptz NOT NULL
  );
  CREATE INDEX IF NOT EXISTS sql_plan_asset_time ON sql_plan_snapshots(asset_id,observed_at DESC);
  CREATE TABLE IF NOT EXISTS trace_spans (
    asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    trace_id text NOT NULL, span_id text NOT NULL, parent_span_id text,
    service text NOT NULL, kind text NOT NULL, started_at timestamptz NOT NULL,
    ended_at timestamptz NOT NULL, status text NOT NULL,
    PRIMARY KEY(asset_id,trace_id,span_id)
  );
  CREATE INDEX IF NOT EXISTS trace_spans_asset_time ON trace_spans(asset_id,started_at DESC);
`;

export class PgStore {
  static async create(connectionString) {
    const pool = new Pool({ connectionString, max: 5, connectionTimeoutMillis: 5000,
      idleTimeoutMillis: 30000, application_name: 'dbsphere-store' });
    pool.on('error', error => console.error('PG18 store connection lost:', error.message));
    try {
      const version = (await pool.query("SELECT current_setting('server_version_num')::int AS version_num")).rows[0].version_num;
      if (Math.floor(version / 10000) !== 18) throw new Error('DbSphere long-term store requires PostgreSQL 18');
      const client = await pool.connect();
      try {
        await client.query('BEGIN');
        await client.query('SELECT pg_advisory_xact_lock(1742026)');
        await client.query(schema);
        const sampleKind = (await client.query(`SELECT c.relkind FROM pg_class c
          WHERE c.oid='samples'::regclass`)).rows[0].relkind;
        if (sampleKind === 'r') {
          await client.query('ALTER TABLE samples RENAME TO samples_legacy');
          await client.query('DROP INDEX IF EXISTS samples_asset_time');
          await client.query(`CREATE TABLE samples (
            id uuid NOT NULL, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
            collected_at timestamptz NOT NULL, status text NOT NULL,
            metrics_json jsonb, error text, PRIMARY KEY(id,collected_at)
          ) PARTITION BY RANGE (collected_at)`);
          await client.query('CREATE INDEX samples_asset_time ON samples(asset_id,collected_at DESC)');
          const months = (await client.query(`SELECT DISTINCT
            date_trunc('month',collected_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS month
            FROM samples_legacy`)).rows;
          for (const row of months) await ensureSamplePartition(client, row.month);
          await client.query('INSERT INTO samples SELECT * FROM samples_legacy');
          await client.query('DROP TABLE samples_legacy');
        }
        const eventKind = (await client.query(`SELECT c.relkind FROM pg_class c
          WHERE c.oid='events'::regclass`)).rows[0].relkind;
        if (eventKind === 'r') {
          await client.query(`INSERT INTO event_keys(id,asset_id,started_at)
            SELECT id,asset_id,started_at FROM events ON CONFLICT(id) DO NOTHING`);
          await client.query('ALTER TABLE evidence DROP CONSTRAINT IF EXISTS evidence_event_id_fkey');
          await client.query('ALTER TABLE events RENAME TO events_legacy');
          await client.query('DROP INDEX IF EXISTS events_asset_status');
          await client.query(`CREATE TABLE events (
            id uuid NOT NULL REFERENCES event_keys(id) ON DELETE CASCADE,
            asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
            kind text NOT NULL, title text NOT NULL, status text NOT NULL,
            started_at timestamptz NOT NULL, ended_at timestamptz,
            event_group text, acknowledged_at timestamptz,
            PRIMARY KEY(id,started_at)
          ) PARTITION BY RANGE (started_at)`);
          await client.query('CREATE INDEX events_asset_status ON events(asset_id,status)');
          const months = (await client.query(`SELECT DISTINCT
            date_trunc('month',started_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS month
            FROM events_legacy`)).rows;
          for (const row of months) await ensureEventPartition(client, row.month);
          await client.query('INSERT INTO events SELECT * FROM events_legacy');
          await client.query(`INSERT INTO open_event_groups(asset_id,event_group,event_id)
            SELECT asset_id,event_group,id FROM events WHERE status='open'`);
          await client.query(`ALTER TABLE evidence ADD CONSTRAINT evidence_event_id_fkey
            FOREIGN KEY(event_id) REFERENCES event_keys(id) ON DELETE CASCADE`);
          await client.query('DROP TABLE events_legacy');
        }
        await ensureSamplePartition(client, new Date());
        await ensureSamplePartition(client, new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth() + 1, 1)));
        await ensureEventPartition(client, new Date());
        await ensureEventPartition(client, new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth() + 1, 1)));
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(1) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(2) ON CONFLICT DO NOTHING');
        for (const rule of DEFAULT_RULES) await client.query(`INSERT INTO alert_rules(id,definition_json)
          VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, [rule.id, JSON.stringify(rule)]);
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(3) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(4) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(5) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(6) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(7) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(8) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(9) ON CONFLICT DO NOTHING');
        for (const template of DEFAULT_INSPECTION_TEMPLATES) {
          await client.query(`INSERT INTO inspection_templates(id,definition_json,updated_at)
            VALUES($1,$2,now()) ON CONFLICT(id) DO NOTHING`, [template.id, JSON.stringify(template)]);
          const current = (await client.query('SELECT definition_json FROM inspection_templates WHERE id=$1',
            [template.id])).rows[0].definition_json;
          const upgraded = upgradeInspectionTemplate(current, template);
          if (upgraded) await client.query('UPDATE inspection_templates SET definition_json=$1,updated_at=now() WHERE id=$2',
            [JSON.stringify(upgraded), template.id]);
        }
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(10) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(11) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(12) ON CONFLICT DO NOTHING');
        await client.query('COMMIT');
      } catch (error) { await client.query('ROLLBACK'); throw error; }
      finally { client.release(); }
      return new PgStore(pool);
    } catch (error) { await pool.end(); throw error; }
  }

  constructor(pool) {
    this.pool = pool; this.kind = 'postgresql18';
    this.samplePartitions = new Set([sampleMonth(new Date()).name,
      sampleMonth(new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth() + 1, 1))).name]);
    this.eventPartitions = new Set([eventMonth(new Date()).name,
      eventMonth(new Date(Date.UTC(new Date().getUTCFullYear(), new Date().getUTCMonth() + 1, 1))).name]);
  }

  async ensureSampleMonth(timestamp) {
    const month = sampleMonth(timestamp);
    if (this.samplePartitions.has(month.name)) return;
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      await client.query('SELECT pg_advisory_xact_lock(1742026)');
      await ensureSamplePartition(client, timestamp);
      await client.query('COMMIT');
      this.samplePartitions.add(month.name);
    } catch (error) { await client.query('ROLLBACK'); throw error; }
    finally { client.release(); }
  }

  async ensureEventMonth(timestamp) {
    const month = eventMonth(timestamp);
    if (this.eventPartitions.has(month.name)) return;
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      await client.query('SELECT pg_advisory_xact_lock(1742026)');
      await ensureEventPartition(client, timestamp);
      await client.query('COMMIT');
      this.eventPartitions.add(month.name);
    } catch (error) { await client.query('ROLLBACK'); throw error; }
    finally { client.release(); }
  }

  async storageStats(retentionDays = 90) {
    const summary = (await this.pool.query(`SELECT count(*)::bigint AS sample_count,
      min(collected_at) AS oldest_sample_at,max(collected_at) AS newest_sample_at,
      count(*) FILTER (WHERE collected_at>=now()-interval '7 days')::bigint AS recent_samples
      FROM samples`)).rows[0];
    const sizes = (await this.pool.query(`SELECT pg_database_size(current_database()) AS database_bytes,
      (SELECT coalesce(sum(pg_total_relation_size(inhrelid)),0) FROM pg_inherits
        WHERE inhparent='samples'::regclass) AS sample_partition_bytes,
      (SELECT coalesce(sum(pg_total_relation_size(inhrelid)),0) FROM pg_inherits
        WHERE inhparent='events'::regclass) AS event_partition_bytes`)).rows[0];
    const eventSummary = (await this.pool.query(`SELECT count(*)::bigint AS event_count,
      count(*) FILTER (WHERE status='open')::bigint AS open_event_count FROM events`)).rows[0];
    const sampleCount = Number(summary.sample_count);
    const samplePartitionBytes = Number(sizes.sample_partition_bytes);
    const estimatedSamplesPerDay = Math.round(Number(summary.recent_samples) / 7);
    return { retentionDays, sampleCount,
      oldestSampleAt: summary.oldest_sample_at, newestSampleAt: summary.newest_sample_at,
      databaseBytes: Number(sizes.database_bytes),
      samplePartitionBytes,
      eventPartitionBytes: Number(sizes.event_partition_bytes),
      eventCount: Number(eventSummary.event_count), openEventCount: Number(eventSummary.open_event_count),
      estimatedSamplesPerDay,
      estimatedSampleBytesPerDay: sampleCount ? Math.round(samplePartitionBytes / sampleCount * estimatedSamplesPerDay) : 0 };
  }

  async aggregateSamples(assetId, from, to, bucketHours = 1) {
    if (!Number.isInteger(bucketHours) || bucketHours < 1 || bucketHours > 24) throw new Error('Invalid aggregation bucket');
    const rows = (await this.pool.query(`SELECT date_bin(make_interval(hours=>$4),collected_at,
      '2000-01-01T00:00:00Z'::timestamptz) AS bucket,
      count(*)::int AS samples,count(*) FILTER (WHERE status='error')::int AS errors,
      round(avg(CASE WHEN jsonb_typeof(metrics_json->'connections')='number'
        THEN (metrics_json->>'connections')::numeric END),1) AS average_connections
      FROM samples WHERE asset_id=$1 AND collected_at >= $2 AND collected_at < $3
      GROUP BY 1 ORDER BY 1`, [assetId, from, to, bucketHours])).rows;
    return rows.map(row => ({ bucket: row.bucket, samples: row.samples,
      errors: row.errors, averageConnections: row.average_connections == null
        ? null : Number(row.average_connections) }));
  }

  async databaseCapacityPoints(assetId, days = 30) {
    const rows = (await this.pool.query(`SELECT DISTINCT ON (day) day, database_bytes, id FROM (
      SELECT date_trunc('day',collected_at AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' AS day,
        (metrics_json->>'database_bytes')::numeric AS database_bytes, id, collected_at
      FROM samples WHERE asset_id=$1 AND collected_at>=now() - $2::int * interval '1 day'
        AND status='ok' AND metrics_json->>'database_bytes' ~ '^[0-9]+$'
    ) AS daily ORDER BY day, collected_at DESC`, [assetId, days])).rows;
    return rows.map(row => ({ day: row.day, databaseBytes: Number(row.database_bytes), sampleId: row.id }));
  }

  async pruneSamples(retentionDays = 90, maxRows = 10000) {
    if (!Number.isInteger(retentionDays) || retentionDays < 1 || retentionDays > 3650 ||
      !Number.isInteger(maxRows) || maxRows < 1 || maxRows > 100000) throw new Error('Invalid retention policy');
    const cutoff = new Date(Date.now() - retentionDays * 86400000).toISOString();
    const removed = await this.pool.query(`WITH protected AS (
      SELECT DISTINCT e.payload_json->>'sampleId' AS sample_id FROM evidence e
      JOIN events v ON v.id=e.event_id
      WHERE v.status='open' AND e.payload_json ? 'sampleId'
    ), doomed AS (
      SELECT id,collected_at FROM samples candidate
      WHERE collected_at<$1 AND NOT EXISTS (SELECT 1 FROM protected
        WHERE protected.sample_id=candidate.id::text)
      ORDER BY collected_at LIMIT $2
    ) DELETE FROM samples s USING doomed d
      WHERE s.id=d.id AND s.collected_at=d.collected_at`, [cutoff, maxRows]);
    return { cutoff, removed: removed.rowCount, moreMayRemain: removed.rowCount === maxRows };
  }

  async pruneResolvedEvents(retentionDays, maxRows = 1000) {
    if (!Number.isInteger(retentionDays) || retentionDays < 1 || retentionDays > 3650 ||
      !Number.isInteger(maxRows) || maxRows < 1 || maxRows > 10000) throw new Error('Invalid event retention policy');
    const cutoff = new Date(Date.now() - retentionDays * 86400000).toISOString();
    const removed = await this.pool.query(`WITH doomed AS (
      SELECT e.id FROM events e WHERE e.status='resolved' AND e.ended_at<$1
        AND NOT EXISTS (SELECT 1 FROM notification_outbox n
          JOIN evidence ev ON ev.id=n.evidence_id
          WHERE ev.event_id=e.id AND n.status IN ('pending','failed'))
      ORDER BY e.ended_at LIMIT $2
    ) DELETE FROM event_keys k USING doomed d WHERE k.id=d.id`, [cutoff, maxRows]);
    return { cutoff, removed: removed.rowCount, moreMayRemain: removed.rowCount === maxRows };
  }

  async health() { await this.pool.query('SELECT 1'); }

  async getSetting(key) {
    return (await this.pool.query('SELECT value FROM settings WHERE key=$1', [key])).rows[0]?.value ?? null;
  }
  async setSetting(key, value) {
    await this.pool.query('INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value', [key, value]);
  }

  async getInspectionTemplates() {
    return (await this.pool.query('SELECT definition_json FROM inspection_templates ORDER BY id')).rows
      .map(row => row.definition_json);
  }
  async getInspectionTemplate(id) {
    return (await this.pool.query('SELECT definition_json FROM inspection_templates WHERE id=$1', [id])).rows[0]?.definition_json ?? null;
  }
  async updateInspectionTemplate(id, input) {
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      const row = (await client.query('SELECT definition_json FROM inspection_templates WHERE id=$1 FOR UPDATE', [id])).rows[0];
      const updated = validateInspectionTemplateUpdate(row?.definition_json, input);
      if (!updated) { await client.query('ROLLBACK'); return null; }
      await client.query('UPDATE inspection_templates SET definition_json=$1,updated_at=now() WHERE id=$2',
        [JSON.stringify(updated), id]);
      await client.query('COMMIT');
      return updated;
    } catch (error) { await client.query('ROLLBACK'); throw error; }
    finally { client.release(); }
  }
  async listInspectionSchedules() {
    return (await this.pool.query('SELECT * FROM inspection_schedules ORDER BY next_due_at')).rows
      .map(row => ({ assetId: row.asset_id, templateId: row.template_id,
        intervalMinutes: row.interval_minutes, nextDueAt: row.next_due_at, enabled: row.enabled }));
  }
  async upsertInspectionSchedule(assetId, templateId, intervalMinutes, enabled = true) {
    const nextDueAt = new Date(Date.now() + intervalMinutes * 60000).toISOString();
    const row = (await this.pool.query(`INSERT INTO inspection_schedules(asset_id,template_id,interval_minutes,next_due_at,enabled)
      SELECT $1,$2,$3,$4,$5 WHERE EXISTS(SELECT 1 FROM assets WHERE id=$1)
      AND EXISTS(SELECT 1 FROM inspection_templates WHERE id=$2)
      ON CONFLICT(asset_id,template_id) DO UPDATE SET interval_minutes=excluded.interval_minutes,
      next_due_at=excluded.next_due_at,enabled=excluded.enabled RETURNING *`,
    [assetId, templateId, intervalMinutes, nextDueAt, enabled])).rows[0];
    return row ? { assetId: row.asset_id, templateId: row.template_id,
      intervalMinutes: row.interval_minutes, nextDueAt: row.next_due_at, enabled: row.enabled } : null;
  }
  async claimDueInspections(now = new Date().toISOString(), limit = 20) {
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      const rows = (await client.query(`SELECT asset_id,template_id,interval_minutes FROM inspection_schedules
        WHERE enabled=true AND next_due_at<=$1 ORDER BY next_due_at LIMIT $2 FOR UPDATE SKIP LOCKED`, [now, limit])).rows;
      for (const row of rows) await client.query(`UPDATE inspection_schedules
        SET next_due_at=$1::timestamptz + interval_minutes * interval '1 minute'
        WHERE asset_id=$2 AND template_id=$3`, [now, row.asset_id, row.template_id]);
      await client.query('COMMIT');
      return rows.map(row => ({ assetId: row.asset_id, templateId: row.template_id }));
    } catch (error) { await client.query('ROLLBACK'); throw error; }
    finally { client.release(); }
  }
  async saveInspectionRun(run) {
    await this.pool.query(`INSERT INTO inspection_runs(id,asset_id,template_id,template_version,sample_id,retry_of,
      started_at,completed_at,summary_json,findings_json,status,error,event_ids_json) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
    [run.id, run.assetId, run.templateId, run.templateVersion, run.sampleId, run.retryOf,
      run.startedAt, run.completedAt, JSON.stringify(run.summary), JSON.stringify(run.findings),
      run.status ?? 'completed', run.error ?? null, JSON.stringify(run.eventIds ?? [])]);
    return run;
  }
  async listInspectionRuns(assetId = null, limit = 50) {
    const rows = (await this.pool.query(`SELECT * FROM inspection_runs WHERE ($1::uuid IS NULL OR asset_id=$1)
      ORDER BY completed_at DESC LIMIT $2`, [assetId, limit])).rows;
    return rows.map(inspectionRun);
  }
  async getInspectionRun(id) {
    const row = (await this.pool.query('SELECT * FROM inspection_runs WHERE id=$1', [id])).rows[0];
    return row ? inspectionRun(row) : null;
  }

  async getAlertRules() {
    return (await this.pool.query('SELECT definition_json FROM alert_rules ORDER BY id')).rows.map(row => row.definition_json);
  }

  async updateAlertRule(id, update) {
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      const row = (await client.query('SELECT definition_json FROM alert_rules WHERE id=$1 FOR UPDATE', [id])).rows[0];
      if (!row) { await client.query('ROLLBACK'); return null; }
      if (!validateRuleUpdate(row.definition_json, update)) { await client.query('ROLLBACK'); return false; }
      const changed = { ...row.definition_json, ...update, version: row.definition_json.version + 1 };
      await client.query('UPDATE alert_rules SET definition_json=$1,updated_at=now() WHERE id=$2', [JSON.stringify(changed), id]);
      await client.query('COMMIT');
      return changed;
    } catch (error) { await client.query('ROLLBACK'); throw error; }
    finally { client.release(); }
  }

  async listActiveAlerts() {
    const rows = (await this.pool.query(`SELECT s.asset_id,a.name AS asset_name,s.rule_id,s.state_json,r.definition_json
      FROM alert_states s JOIN assets a ON a.id=s.asset_id JOIN alert_rules r ON r.id=s.rule_id
      WHERE s.state_json->>'status'='firing'`)).rows;
    return rows.map(row => ({ assetId: row.asset_id, assetName: row.asset_name,
      ruleId: row.rule_id, rule: row.definition_json, state: row.state_json }));
  }

  async listSilences() {
    const rows = (await this.pool.query('SELECT * FROM silences ORDER BY created_at DESC LIMIT 200')).rows;
    return rows.map(publicSilence);
  }

  async createSilence(input) {
    if (input.assetId && !await this.getInternalAsset(input.assetId)) return null;
    if (input.ruleId && !(await this.pool.query('SELECT 1 FROM alert_rules WHERE id=$1', [input.ruleId])).rowCount) return null;
    const id = randomUUID();
    const createdAt = new Date().toISOString();
    await this.pool.query(`INSERT INTO silences(id,kind,asset_id,rule_id,starts_at,ends_at,reason,created_at)
      VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, [id, input.kind, input.assetId,
      input.ruleId, input.startsAt, input.endsAt, input.reason, createdAt]);
    return publicSilence((await this.pool.query('SELECT * FROM silences WHERE id=$1', [id])).rows[0]);
  }

  async revokeSilence(id) {
    const changed = await this.pool.query('UPDATE silences SET revoked_at=now() WHERE id=$1 AND revoked_at IS NULL', [id]);
    return changed.rowCount ? publicSilence((await this.pool.query('SELECT * FROM silences WHERE id=$1', [id])).rows[0]) : null;
  }

  async listDueNotifications(now = new Date().toISOString(), limit = 50) {
    const rows = (await this.pool.query(`SELECT n.id,n.channel,n.attempts,e.id AS evidence_id,e.kind,e.observed_at,
      e.payload_json,v.id AS event_id,v.title,v.status AS event_status,a.id AS asset_id,a.name AS asset_name
      FROM notification_outbox n JOIN evidence e ON e.id=n.evidence_id
      JOIN events v ON v.id=e.event_id JOIN assets a ON a.id=v.asset_id
      WHERE n.status='pending' AND n.next_attempt_at<=$1
      ORDER BY n.next_attempt_at,n.id LIMIT $2`, [now, limit])).rows;
    return rows.map(row => ({ id: row.id, channel: row.channel, attempts: row.attempts,
      evidenceId: row.evidence_id, kind: row.kind, observedAt: row.observed_at,
      payload: row.payload_json, eventId: row.event_id, eventTitle: row.title,
      eventStatus: row.event_status, assetId: row.asset_id, assetName: row.asset_name }));
  }

  async listNotificationHistory(limit = 100) {
    const rows = (await this.pool.query(`SELECT n.id,n.channel,n.status,n.attempts,n.next_attempt_at,
      n.delivered_at,n.last_error,e.kind,e.observed_at,v.id AS event_id,a.name AS asset_name
      FROM notification_outbox n JOIN evidence e ON e.id=n.evidence_id
      JOIN events v ON v.id=e.event_id JOIN assets a ON a.id=v.asset_id
      ORDER BY e.observed_at DESC,n.id DESC LIMIT $1`, [limit])).rows;
    return rows.map(row => ({ id: row.id, channel: row.channel, status: row.status,
      attempts: row.attempts, nextAttemptAt: row.next_attempt_at,
      deliveredAt: row.delivered_at, lastError: row.last_error, kind: row.kind,
      observedAt: row.observed_at, eventId: row.event_id, assetName: row.asset_name }));
  }

  async completeNotification(id, outcome, now = new Date().toISOString()) {
    if (outcome === 'delivered' || outcome === 'skipped') {
      return (await this.pool.query(`UPDATE notification_outbox SET status=$1,delivered_at=$2
        WHERE id=$3 AND status='pending'`, [outcome, now, id])).rowCount > 0;
    }
    const row = (await this.pool.query('SELECT attempts FROM notification_outbox WHERE id=$1 AND status=$2', [id, 'pending'])).rows[0];
    if (!row) return false;
    const attempts = row.attempts + 1;
    const next = new Date(Date.parse(now) + Math.min(3600, 30 * 2 ** (attempts - 1)) * 1000).toISOString();
    return (await this.pool.query(`UPDATE notification_outbox SET status=$1,attempts=$2,
      next_attempt_at=$3,last_error=$4 WHERE id=$5 AND status='pending'`,
    [attempts >= 5 ? 'failed' : 'pending', attempts, next, String(outcome).slice(0, 300), id])).rowCount > 0;
  }

  async retryNotification(id, now = new Date().toISOString()) {
    return (await this.pool.query(`UPDATE notification_outbox SET status='pending',attempts=0,
      next_attempt_at=$1,last_error=NULL WHERE id=$2 AND status='failed'`, [now, id])).rowCount > 0;
  }

  async queueEscalations(now = new Date().toISOString(), afterMinutes = 15) {
    const cutoff = new Date(Date.parse(now) - afterMinutes * 60000).toISOString();
    const client = await this.pool.connect();
    let queued = 0;
    try {
      await client.query('BEGIN');
      const alerts = (await client.query(`SELECT s.asset_id,s.rule_id,s.state_json,r.definition_json
        FROM alert_states s JOIN alert_rules r ON r.id=s.rule_id
        WHERE s.state_json->>'status'='firing'`)).rows;
      for (const alert of alerts) {
        const rule = alert.definition_json;
        const state = alert.state_json;
        if (rule.severity !== 'critical' || state.mutedBy) continue;
        const event = (await client.query(`SELECT id FROM events WHERE id=$1 AND status='open'
          AND acknowledged_at IS NULL AND started_at<=$2 FOR UPDATE`, [state.eventId, cutoff])).rows[0];
        if (!event) continue;
        const silenced = (await client.query(`SELECT 1 FROM silences WHERE revoked_at IS NULL
          AND starts_at<=$1 AND ends_at>$1 AND (asset_id IS NULL OR asset_id=$2)
          AND (rule_id IS NULL OR rule_id=$3) LIMIT 1`, [now, alert.asset_id, alert.rule_id])).rowCount;
        if (silenced) continue;
        const existing = (await client.query(`SELECT 1 FROM evidence WHERE event_id=$1
          AND kind='alert_escalation' AND payload_json->>'ruleId'=$2 LIMIT 1`, [event.id, alert.rule_id])).rowCount;
        if (existing) continue;
        const evidenceId = randomUUID();
        await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
          VALUES($1,$2,'alert_escalation',$3,$4)`, [evidenceId, event.id, now,
          JSON.stringify({ ruleId: alert.rule_id, severity: rule.severity, source: rule.source,
            value: state.lastValue, afterMinutes })]);
        for (const channel of ['webhook', 'email']) await client.query(`INSERT INTO notification_outbox
          (id,evidence_id,channel,next_attempt_at) VALUES($1,$2,$3,$4)`, [randomUUID(), evidenceId, channel, now]);
        queued++;
      }
      await client.query('COMMIT');
      return queued;
    } catch (error) { await client.query('ROLLBACK'); throw error; }
    finally { client.release(); }
  }

  async listAssets() {
    return (await this.pool.query('SELECT * FROM assets ORDER BY created_at DESC')).rows.map(publicAsset);
  }
  async getInternalAsset(id) {
    return (await this.pool.query('SELECT * FROM assets WHERE id=$1', [id])).rows[0] ?? null;
  }
  async getAsset(id) { return publicAsset(await this.getInternalAsset(id)); }

  async addAsset(a, encryptedPassword, encryptedPoolerUrl = null) {
    const id = randomUUID();
    await this.pool.query(`INSERT INTO assets
      (id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,linux_host,linux_exporter_port,pooler_url_enc,k8s_cluster,k8s_namespace,k8s_workload,k8s_pod,k8s_node,created_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, [
      id, a.name, a.host, a.port, a.database, a.username, encryptedPassword,
      a.sslMode, a.runtime, a.linuxHost || null, a.linuxExporterPort || null, encryptedPoolerUrl, a.k8sCluster || null,
      a.namespace || null, a.workload || null, a.pod || null, a.node || null,
      new Date().toISOString()
    ]);
    return this.getAsset(id);
  }

  async updateAsset(id, a, encryptedPassword, encryptedPoolerUrl = null) {
    const updated = await this.pool.query(`UPDATE assets SET
      name=$1,host=$2,port=$3,database_name=$4,username=$5,password_enc=$6,ssl_mode=$7,runtime=$8,
      linux_host=$9,linux_exporter_port=$10,pooler_url_enc=$11,k8s_cluster=$12,k8s_namespace=$13,k8s_workload=$14,k8s_pod=$15,k8s_node=$16,
      last_sample_at=NULL,last_status='pending',last_error=NULL,version_num=NULL,role=NULL
      WHERE id=$17 RETURNING id`, [
      a.name, a.host, a.port, a.database, a.username, encryptedPassword, a.sslMode, a.runtime,
      a.linuxHost || null, a.linuxExporterPort || null, encryptedPoolerUrl, a.k8sCluster || null, a.namespace || null,
      a.workload || null, a.pod || null, a.node || null, id
    ]);
    return updated.rowCount ? this.getAsset(id) : null;
  }

  async deleteAsset(id) {
    return (await this.pool.query('DELETE FROM assets WHERE id=$1', [id])).rowCount > 0;
  }

  async applyAlerts(client, assetId, result, previous, error, sampleId, now, onlyRuleId = null) {
    const rules = (await client.query('SELECT definition_json FROM alert_rules ORDER BY id')).rows.map(row => row.definition_json);
    const states = new Map((await client.query('SELECT rule_id,state_json FROM alert_states WHERE asset_id=$1', [assetId])).rows
      .map(row => [row.rule_id, row.state_json]));
    const silences = (await client.query(`SELECT id,rule_id FROM silences WHERE revoked_at IS NULL
      AND starts_at<=$1 AND ends_at>$1 AND (asset_id IS NULL OR asset_id=$2)
      ORDER BY created_at DESC`, [now, assetId])).rows;
    for (const rule of rules) {
      if (onlyRuleId && rule.id !== onlyRuleId) continue;
      const old = states.get(rule.id) ?? null;
      if (!rule.enabled) {
        if (old?.status === 'firing' && old.eventId) await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
          VALUES($1,$2,'alert_disabled',$3,$4)`, [randomUUID(), old.eventId, now,
          JSON.stringify({ ruleId: rule.id, ruleVersion: rule.version, sampleId })]);
        if (old) await client.query('DELETE FROM alert_states WHERE asset_id=$1 AND rule_id=$2', [assetId, rule.id]);
        continue;
      }
      const signal = readSignal(rule.metric, result, previous, error, Date.parse(now));
      const { next, transition } = nextAlertState(rule, signal, old, now);
      if (next) next.mutedBy = silences.find(item => !item.rule_id || item.rule_id === rule.id)?.id ?? null;
      if (transition === 'firing') {
        let eventId = (await client.query(`SELECT id FROM events WHERE asset_id=$1 AND event_group=$2
          AND status='open' FOR UPDATE`, [assetId, rule.group])).rows[0]?.id;
        if (!eventId) {
          eventId = randomUUID();
          await client.query('INSERT INTO event_keys(id,asset_id,started_at) VALUES($1,$2,$3)', [eventId, assetId, now]);
          await client.query(`INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
            VALUES($1,$2,'alert_incident',$3,'open',$4,$5)`, [eventId, assetId, rule.title, now, rule.group]);
          await client.query(`INSERT INTO open_event_groups(asset_id,event_group,event_id)
            VALUES($1,$2,$3)`, [assetId, rule.group, eventId]);
        }
        next.eventId = eventId;
      }
      const transitions = [];
      if (transition === 'firing') transitions.push(next.mutedBy ? 'suppressed' : 'firing');
      else if (transition) transitions.push(transition);
      if (old?.status === 'firing' && next?.status === 'firing' && old.mutedBy !== next.mutedBy) {
        transitions.push(next.mutedBy ? 'suppressed' : 'unsuppressed');
      }
      for (const change of transitions) if (next?.eventId || old?.eventId) {
        const evidenceId = randomUUID();
        await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
          VALUES($1,$2,$3,$4,$5)`, [evidenceId, next?.eventId ?? old.eventId, `alert_${change}`, now,
          JSON.stringify({ ruleId: rule.id, ruleVersion: rule.version, metric: rule.metric,
            severity: rule.severity, source: signal.source, value: signal.value,
            threshold: rule.threshold, operator: rule.operator, sampleId,
            detail: signal.detail, reason: signal.reason,
            silenceId: next?.mutedBy ?? old?.mutedBy ?? null })]);
        if (['firing', 'unsuppressed', 'recovered'].includes(change) &&
          !(change === 'recovered' && old?.mutedBy)) {
          for (const channel of ['webhook', 'email']) await client.query(`INSERT INTO notification_outbox
            (id,evidence_id,channel,next_attempt_at) VALUES($1,$2,$3,$4)`, [randomUUID(), evidenceId, channel, now]);
        }
      }
      if (next) await client.query(`INSERT INTO alert_states(asset_id,rule_id,state_json) VALUES($1,$2,$3)
        ON CONFLICT(asset_id,rule_id) DO UPDATE SET state_json=excluded.state_json`, [assetId, rule.id, JSON.stringify(next)]);
      else if (old) await client.query('DELETE FROM alert_states WHERE asset_id=$1 AND rule_id=$2', [assetId, rule.id]);
    }
  }

  async checkStale(assetId, now = new Date().toISOString()) {
    await this.ensureEventMonth(now);
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      const asset = (await client.query('SELECT created_at,last_sample_at FROM assets WHERE id=$1 FOR UPDATE', [assetId])).rows[0];
      if (!asset) { await client.query('ROLLBACK'); return; }
      const interval = Math.max(15, Number(process.env.DBSPHERE_COLLECT_INTERVAL_SECONDS) || 60);
      const last = asset.last_sample_at ?? asset.created_at;
      const missed = Math.max(0, (new Date(now).getTime() - new Date(last).getTime()) / 1000 / interval);
      await this.applyAlerts(client, assetId, { collectorMissedIntervals: missed }, null, null, null, now, 'collector_stale');
      await client.query('COMMIT');
    } catch (error) { await client.query('ROLLBACK'); throw error; }
    finally { client.release(); }
  }

  async resolveInactiveEvents(client, assetId, error, result, now) {
    const currentGroup = error && readSignal('pg.unavailable', result, null, error).value === 1 ? 'availability' : 'telemetry';
    const firing = new Set((await client.query(`SELECT state_json->>'eventId' AS event_id FROM alert_states
      WHERE asset_id=$1 AND state_json->>'status'='firing'`, [assetId])).rows.map(row => row.event_id));
    const open = (await client.query("SELECT id,event_group FROM events WHERE asset_id=$1 AND status='open' AND kind IN ('collection_failure','alert_incident')", [assetId])).rows;
    for (const event of open) {
      if ((error && event.event_group === currentGroup) || firing.has(event.id)) continue;
      await client.query("UPDATE events SET status='resolved',ended_at=$1 WHERE id=$2", [now, event.id]);
      await client.query('DELETE FROM open_event_groups WHERE event_id=$1', [event.id]);
    }
  }

  async recordResult(assetId, result, error = null) {
    const now = new Date().toISOString();
    await this.ensureSampleMonth(now);
    await this.ensureEventMonth(now);
    const status = error ? 'error' : 'ok';
    const sampleId = randomUUID();
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      const prior = (await client.query(`SELECT metrics_json,error FROM samples WHERE asset_id=$1
        ORDER BY collected_at DESC LIMIT 1`, [assetId])).rows[0];
      const previous = prior?.metrics_json ?? null;
      await client.query(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json,error)
        VALUES($1,$2,$3,$4,$5,$6)`, [sampleId, assetId, now, status, JSON.stringify(result), error]);
      await client.query(`UPDATE assets SET last_sample_at=$1,last_status=$2,last_error=$3,
        version_num=COALESCE($4,version_num),role=COALESCE($5,role) WHERE id=$6`,
      [now, status, error, result?.version_num ?? null, result?.role ?? null, assetId]);
      if (error) {
        const group = readSignal('pg.unavailable', result, previous, error).value === 1 ? 'availability' : 'telemetry';
        const open = (await client.query(`SELECT id FROM events WHERE asset_id=$1 AND event_group=$2
          AND status='open' FOR UPDATE`, [assetId, group])).rows[0];
        const eventId = open?.id ?? randomUUID();
        if (!open) {
          await client.query('INSERT INTO event_keys(id,asset_id,started_at) VALUES($1,$2,$3)', [eventId, assetId, now]);
          await client.query(`INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
            VALUES($1,$2,'collection_failure',$3,'open',$4,$5)`, [eventId, assetId,
            group === 'availability' ? 'PostgreSQL 无法连接' : 'PostgreSQL 采集失败', now, group]);
          await client.query(`INSERT INTO open_event_groups(asset_id,event_group,event_id)
            VALUES($1,$2,$3)`, [assetId, group, eventId]);
        }
        const collectionEvidenceId = randomUUID();
        await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
          VALUES($1,$2,'collection_error',$3,$4)`, [collectionEvidenceId, eventId, now,
          JSON.stringify({ sampleId, error })]);
        if (result?.k8s) {
          const k8sEvidenceId = randomUUID();
          await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
            VALUES($1,$2,'k8s_state',$3,$4)`, [k8sEvidenceId, eventId, now, JSON.stringify(result.k8s)]);
          const candidate = oomCandidate(result, error, collectionEvidenceId, k8sEvidenceId);
          const hasCandidate = (await client.query(`SELECT 1 FROM evidence
            WHERE event_id=$1 AND kind='root_cause_candidate' LIMIT 1`, [eventId])).rowCount > 0;
          if (candidate && !hasCandidate) await client.query(`INSERT INTO evidence
            (id,event_id,kind,observed_at,payload_json) VALUES($1,$2,'root_cause_candidate',$3,$4)`,
          [randomUUID(), eventId, now, JSON.stringify(candidate)]);
        }
        if (result?.linux) {
          await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
            VALUES($1,$2,'linux_state',$3,$4)`, [randomUUID(), eventId, now, JSON.stringify(result.linux)]);
        }
        if (result?.pgbouncer) {
          await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
            VALUES($1,$2,'pgbouncer_state',$3,$4)`, [randomUUID(), eventId, now, JSON.stringify(result.pgbouncer)]);
        }
      } else if (prior?.error) {
        const group = readSignal('pg.unavailable', previous, null, prior.error).value === 1 ? 'availability' : 'telemetry';
        const open = (await client.query(`SELECT id FROM events WHERE asset_id=$1 AND event_group=$2
          AND status='open' FOR UPDATE`, [assetId, group])).rows[0];
        if (open) await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
          VALUES($1,$2,'collection_recovered',$3,$4)`, [randomUUID(), open.id, now, JSON.stringify({ sampleId })]);
      }
      await this.applyAlerts(client, assetId, result, previous, error, sampleId, now);
      await this.resolveInactiveEvents(client, assetId, error, result, now);
      await client.query('COMMIT');
      return { sampleId, status, collectedAt: now };
    } catch (cause) { await client.query('ROLLBACK'); throw cause; }
    finally { client.release(); }
  }

  async listSamples(assetId, limit = 100) {
    const rows = (await this.pool.query(`SELECT * FROM samples WHERE asset_id=$1
      ORDER BY collected_at DESC LIMIT $2`, [assetId, limit])).rows;
    return rows.map(r => ({ id: r.id, assetId: r.asset_id, collectedAt: r.collected_at,
      status: r.status, metrics: r.metrics_json, error: r.error }));
  }

  async getSamplesByIds(assetId, ids) {
    if (!ids.length) return [];
    const rows = (await this.pool.query(`SELECT * FROM samples WHERE asset_id=$1 AND id=ANY($2::uuid[])
      ORDER BY collected_at DESC`, [assetId, ids.slice(0, 200)])).rows;
    return rows.map(r => ({ id: r.id, assetId: r.asset_id, collectedAt: r.collected_at,
      status: r.status, metrics: r.metrics_json, error: r.error }));
  }

  async listSamplesInWindow(assetId, from, to, limit = 100) {
    const rows = (await this.pool.query(`SELECT * FROM samples WHERE asset_id=$1
      AND collected_at>=$2 AND collected_at<$3 ORDER BY collected_at DESC LIMIT $4`,
    [assetId, from, to, limit])).rows;
    return rows.map(r => ({ id: r.id, assetId: r.asset_id, collectedAt: r.collected_at,
      status: r.status, metrics: r.metrics_json, error: r.error }));
  }

  async importSlowLogs(assetId, entries) {
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      let imported = 0;
      for (const row of entries) {
        const result = await client.query(`INSERT INTO slow_log_entries
          (id,asset_id,source_key,observed_at,duration_ms,query_id,source_format,imported_at)
          VALUES($1,$2,$3,$4,$5,$6,$7,now()) ON CONFLICT(asset_id,source_key) DO NOTHING`,
        [row.id, assetId, row.sourceKey, row.observedAt, row.durationMs, row.queryId, row.sourceFormat]);
        imported += result.rowCount;
      }
      await client.query('COMMIT');
      return imported;
    } catch (error) { await client.query('ROLLBACK'); throw error; }
    finally { client.release(); }
  }

  async listSlowLogs(assetId, from, to, limit = 100) {
    const rows = (await this.pool.query(`SELECT id,asset_id,observed_at,duration_ms,query_id,source_format
      FROM slow_log_entries WHERE asset_id=$1 AND observed_at>=$2 AND observed_at<$3
      ORDER BY observed_at DESC LIMIT $4`, [assetId, from, to, limit])).rows;
    return rows.map(row => ({ id: row.id, assetId: row.asset_id, observedAt: row.observed_at,
      durationMs: Number(row.duration_ms), queryId: row.query_id, sourceFormat: row.source_format }));
  }

  async getSlowLog(assetId, id) {
    const row = (await this.pool.query(`SELECT id,asset_id,observed_at,duration_ms,query_id,source_format
      FROM slow_log_entries WHERE asset_id=$1 AND id=$2`, [assetId, id])).rows[0];
    return row ? { id: row.id, assetId: row.asset_id, observedAt: row.observed_at,
      durationMs: Number(row.duration_ms), queryId: row.query_id,
      sourceFormat: row.source_format } : null;
  }

  async saveSqlPlan(assetId, plan) {
    await this.pool.query(`INSERT INTO sql_plan_snapshots
      (id,asset_id,query_id,observed_at,source,shape_hash,summary_json,imported_at)
      VALUES($1,$2,$3,$4,$5,$6,$7,now())`, [plan.id, assetId, plan.queryId,
      plan.observedAt, plan.source, plan.shapeHash, JSON.stringify(plan.summary)]);
    return plan;
  }

  async listSqlPlans(assetId, limit = 50) {
    const rows = (await this.pool.query(`SELECT * FROM sql_plan_snapshots WHERE asset_id=$1
      ORDER BY observed_at DESC LIMIT $2`, [assetId, limit])).rows;
    return rows.map(row => ({ id: row.id, assetId: row.asset_id, queryId: row.query_id,
      observedAt: row.observed_at, source: row.source, shapeHash: row.shape_hash,
      summary: row.summary_json }));
  }

  async getSqlPlan(assetId, id) {
    const row = (await this.pool.query('SELECT * FROM sql_plan_snapshots WHERE asset_id=$1 AND id=$2',
      [assetId, id])).rows[0];
    return row ? { id: row.id, assetId: row.asset_id, queryId: row.query_id,
      observedAt: row.observed_at, source: row.source, shapeHash: row.shape_hash,
      summary: row.summary_json } : null;
  }

  async listEvents() {
    const rows = (await this.pool.query(`SELECT e.*, a.name AS asset_name FROM events e
      JOIN assets a ON a.id=e.asset_id ORDER BY e.started_at DESC LIMIT 200`)).rows;
    return rows.map(r => ({ id: r.id, assetId: r.asset_id, assetName: r.asset_name,
      kind: r.kind, eventGroup: r.event_group, title: r.title, status: r.status,
      startedAt: r.started_at, endedAt: r.ended_at, acknowledgedAt: r.acknowledged_at }));
  }

  async acknowledgeEvent(id) {
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      const event = (await client.query('SELECT id,status,acknowledged_at FROM events WHERE id=$1 FOR UPDATE', [id])).rows[0];
      if (!event) { await client.query('ROLLBACK'); return null; }
      if (event.status === 'open' && !event.acknowledged_at) {
        const now = new Date().toISOString();
        await client.query('UPDATE events SET acknowledged_at=$1 WHERE id=$2', [now, id]);
        await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
          VALUES($1,$2,'event_acknowledged',$3,$4)`, [randomUUID(), id, now, JSON.stringify({ actor: 'admin' })]);
      }
      await client.query('COMMIT');
      return this.getEvent(id);
    } catch (error) { await client.query('ROLLBACK'); throw error; }
    finally { client.release(); }
  }

  async recordRcaDecision(id, code, decision, supports, reason) {
    const inserted = await this.pool.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
      SELECT $2,id,'rca_decision',now(),$3 FROM events WHERE id=$1 RETURNING id`,
    [id, randomUUID(), JSON.stringify({ code, decision, supports, reason, actor: 'admin' })]);
    return inserted.rowCount ? this.getEvent(id) : null;
  }

  async getEvent(id) {
    const row = (await this.pool.query(`SELECT e.*, a.name AS asset_name FROM events e
      JOIN assets a ON a.id=e.asset_id WHERE e.id=$1`, [id])).rows[0];
    if (!row) return null;
    const evidence = (await this.pool.query('SELECT * FROM evidence WHERE event_id=$1 ORDER BY observed_at', [id])).rows
      .map(e => ({ id: e.id, kind: e.kind, observedAt: e.observed_at, payload: e.payload_json }));
    return { id: row.id, assetId: row.asset_id, assetName: row.asset_name,
      kind: row.kind, eventGroup: row.event_group, title: row.title, status: row.status,
      startedAt: row.started_at, endedAt: row.ended_at, acknowledgedAt: row.acknowledged_at, evidence };
  }

  async saveTraceSpans(spans) {
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      let imported = 0;
      for (const span of spans) {
        const result = await client.query(`INSERT INTO trace_spans
          (asset_id,trace_id,span_id,parent_span_id,service,kind,started_at,ended_at,status)
          VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
          ON CONFLICT(asset_id,trace_id,span_id) DO NOTHING`,
        [span.assetId, span.traceId, span.spanId, span.parentSpanId, span.service,
          span.kind, span.start, span.end, span.status]);
        imported += result.rowCount;
      }
      await client.query('COMMIT');
      return imported;
    } catch (error) { await client.query('ROLLBACK'); throw error; }
    finally { client.release(); }
  }

  async listTraceSpans(assetId, from, to, limit = 200) {
    const rows = (await this.pool.query(`SELECT * FROM trace_spans
      WHERE asset_id=$1 AND started_at<$3 AND ended_at>=$2
      ORDER BY started_at DESC LIMIT $4`, [assetId, from, to, limit])).rows;
    return rows.map(traceSpan);
  }

  async getTraceSpans(assetId, traceId) {
    const rows = (await this.pool.query(`SELECT * FROM trace_spans WHERE asset_id=$1 AND trace_id=$2
      ORDER BY started_at LIMIT 256`, [assetId, traceId])).rows;
    return rows.map(traceSpan);
  }

  async pruneTraceSpans(retentionDays) {
    if (!Number.isInteger(retentionDays) || retentionDays < 1 || retentionDays > 3650) throw new Error('Invalid trace retention');
    return (await this.pool.query(`DELETE FROM trace_spans WHERE ended_at < now() - $1::interval`,
      [`${retentionDays} days`])).rowCount;
  }

  async close() { await this.pool.end(); }
}

const traceSpan = row => ({ assetId: row.asset_id, traceId: row.trace_id,
  spanId: row.span_id, parentSpanId: row.parent_span_id, service: row.service,
  kind: row.kind, start: row.started_at, end: row.ended_at, status: row.status });
