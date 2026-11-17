import pg from 'pg';
import { randomUUID } from 'node:crypto';
import { oomCandidate } from './incidents.js';
import { publicAsset, publicSilence } from './store.js';
import { DEFAULT_RULES, readSignal, validateRuleUpdate } from './alert-rules.js';
import { nextAlertState } from './alert-state.js';

const { Pool } = pg;

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
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    collected_at timestamptz NOT NULL, status text NOT NULL,
    metrics_json jsonb, error text
  );
  CREATE INDEX IF NOT EXISTS samples_asset_time ON samples(asset_id, collected_at DESC);
  CREATE TABLE IF NOT EXISTS events (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    kind text NOT NULL, title text NOT NULL, status text NOT NULL,
    started_at timestamptz NOT NULL, ended_at timestamptz, event_group text, acknowledged_at timestamptz
  );
  ALTER TABLE events ADD COLUMN IF NOT EXISTS event_group text;
  ALTER TABLE events ADD COLUMN IF NOT EXISTS acknowledged_at timestamptz;
  UPDATE events SET event_group='telemetry' WHERE event_group IS NULL;
  CREATE INDEX IF NOT EXISTS events_asset_status ON events(asset_id, status);
  DROP INDEX IF EXISTS events_one_open_kind;
  CREATE UNIQUE INDEX IF NOT EXISTS events_one_open_group ON events(asset_id,event_group) WHERE status='open';
  CREATE TABLE IF NOT EXISTS evidence (
    id uuid PRIMARY KEY, event_id uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
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
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(1) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(2) ON CONFLICT DO NOTHING');
        for (const rule of DEFAULT_RULES) await client.query(`INSERT INTO alert_rules(id,definition_json)
          VALUES($1,$2) ON CONFLICT(id) DO NOTHING`, [rule.id, JSON.stringify(rule)]);
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(3) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(4) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(5) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(6) ON CONFLICT DO NOTHING');
        await client.query('INSERT INTO dbsphere_schema_migrations(version) VALUES(7) ON CONFLICT DO NOTHING');
        await client.query('COMMIT');
      } catch (error) { await client.query('ROLLBACK'); throw error; }
      finally { client.release(); }
      return new PgStore(pool);
    } catch (error) { await pool.end(); throw error; }
  }

  constructor(pool) { this.pool = pool; this.kind = 'postgresql18'; }

  async health() { await this.pool.query('SELECT 1'); }

  async getSetting(key) {
    return (await this.pool.query('SELECT value FROM settings WHERE key=$1', [key])).rows[0]?.value ?? null;
  }
  async setSetting(key, value) {
    await this.pool.query('INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=excluded.value', [key, value]);
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
          await client.query(`INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
            VALUES($1,$2,'alert_incident',$3,'open',$4,$5)`, [eventId, assetId, rule.title, now, rule.group]);
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
    }
  }

  async recordResult(assetId, result, error = null) {
    const now = new Date().toISOString();
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
        if (!open) await client.query(`INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
          VALUES($1,$2,'collection_failure',$3,'open',$4,$5)`, [eventId, assetId,
          group === 'availability' ? 'PostgreSQL 无法连接' : 'PostgreSQL 采集失败', now, group]);
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

  async close() { await this.pool.end(); }
}
