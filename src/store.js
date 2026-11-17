import { DatabaseSync } from 'node:sqlite';
import { randomUUID } from 'node:crypto';
import { join } from 'node:path';
import { oomCandidate } from './incidents.js';
import { DEFAULT_RULES, readSignal, validateRuleUpdate } from './alert-rules.js';
import { nextAlertState } from './alert-state.js';

export const publicAsset = (row) => row && ({
  id: row.id, name: row.name, host: row.host, port: row.port,
  database: row.database_name, username: row.username, sslMode: row.ssl_mode,
  runtime: row.runtime, linuxHost: row.linux_host,
  linuxExporterPort: row.linux_exporter_port,
  poolerConfigured: Boolean(row.pooler_url_enc),
  k8sCluster: row.k8s_cluster, namespace: row.k8s_namespace,
  workload: row.k8s_workload, pod: row.k8s_pod, node: row.k8s_node,
  createdAt: row.created_at, lastSampleAt: row.last_sample_at,
  lastStatus: row.last_status, lastError: row.last_error,
  versionNum: row.version_num, role: row.role,
  pg14Eol: row.version_num !== null && Math.floor(row.version_num / 10000) === 14
});

export class Store {
  constructor(dataDir) {
    this.kind = 'sqlite-preview';
    this.db = new DatabaseSync(join(dataDir, 'dbsphere.sqlite'));
    this.db.exec(`
      PRAGMA journal_mode=WAL;
      PRAGMA foreign_keys=ON;
      CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
      CREATE TABLE IF NOT EXISTS assets (
        id TEXT PRIMARY KEY, name TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL,
        database_name TEXT NOT NULL, username TEXT NOT NULL, password_enc TEXT NOT NULL,
        ssl_mode TEXT NOT NULL, runtime TEXT NOT NULL, linux_host TEXT, linux_exporter_port INTEGER, pooler_url_enc TEXT, k8s_cluster TEXT,
        k8s_namespace TEXT, k8s_workload TEXT, k8s_pod TEXT, k8s_node TEXT,
        created_at TEXT NOT NULL, last_sample_at TEXT, last_status TEXT NOT NULL DEFAULT 'pending',
        last_error TEXT, version_num INTEGER, role TEXT
      );
      CREATE TABLE IF NOT EXISTS samples (
        id TEXT PRIMARY KEY, asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
        collected_at TEXT NOT NULL, status TEXT NOT NULL, metrics_json TEXT, error TEXT
      );
      CREATE INDEX IF NOT EXISTS samples_asset_time ON samples(asset_id, collected_at DESC);
      CREATE TABLE IF NOT EXISTS events (
        id TEXT PRIMARY KEY, asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
        kind TEXT NOT NULL, title TEXT NOT NULL, status TEXT NOT NULL,
        started_at TEXT NOT NULL, ended_at TEXT, event_group TEXT, acknowledged_at TEXT
      );
      CREATE INDEX IF NOT EXISTS events_asset_status ON events(asset_id, status);
      CREATE TABLE IF NOT EXISTS evidence (
        id TEXT PRIMARY KEY, event_id TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
        kind TEXT NOT NULL, observed_at TEXT NOT NULL, payload_json TEXT NOT NULL
      );
      CREATE INDEX IF NOT EXISTS evidence_event_time ON evidence(event_id, observed_at);
      CREATE TABLE IF NOT EXISTS alert_rules (
        id TEXT PRIMARY KEY, definition_json TEXT NOT NULL, updated_at TEXT NOT NULL
      );
      CREATE TABLE IF NOT EXISTS alert_states (
        asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
        rule_id TEXT NOT NULL REFERENCES alert_rules(id), state_json TEXT NOT NULL,
        PRIMARY KEY(asset_id,rule_id)
      );
    `);
    if (!this.db.prepare('PRAGMA table_info(assets)').all().some(column => column.name === 'linux_exporter_port')) {
      this.db.exec('ALTER TABLE assets ADD COLUMN linux_exporter_port INTEGER');
    }
    if (!this.db.prepare('PRAGMA table_info(assets)').all().some(column => column.name === 'pooler_url_enc')) {
      this.db.exec('ALTER TABLE assets ADD COLUMN pooler_url_enc TEXT');
    }
    if (!this.db.prepare('PRAGMA table_info(events)').all().some(column => column.name === 'event_group')) {
      this.db.exec('ALTER TABLE events ADD COLUMN event_group TEXT');
    }
    if (!this.db.prepare('PRAGMA table_info(events)').all().some(column => column.name === 'acknowledged_at')) {
      this.db.exec('ALTER TABLE events ADD COLUMN acknowledged_at TEXT');
    }
    this.db.exec("UPDATE events SET event_group='telemetry' WHERE event_group IS NULL");
    this.db.exec("CREATE UNIQUE INDEX IF NOT EXISTS events_one_open_group ON events(asset_id,event_group) WHERE status='open'");
    const insertRule = this.db.prepare('INSERT OR IGNORE INTO alert_rules(id,definition_json,updated_at) VALUES(?,?,?)');
    for (const rule of DEFAULT_RULES) insertRule.run(rule.id, JSON.stringify(rule), new Date().toISOString());
  }

  getSetting(key) { return this.db.prepare('SELECT value FROM settings WHERE key=?').get(key)?.value ?? null; }
  health() { this.db.prepare('SELECT 1').get(); }
  setSetting(key, value) { this.db.prepare('INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value').run(key, value); }

  getAlertRules() {
    return this.db.prepare('SELECT definition_json FROM alert_rules ORDER BY id').all().map(row => JSON.parse(row.definition_json));
  }

  updateAlertRule(id, update) {
    const row = this.db.prepare('SELECT definition_json FROM alert_rules WHERE id=?').get(id);
    if (!row) return null;
    const rule = JSON.parse(row.definition_json);
    if (!validateRuleUpdate(rule, update)) return false;
    const changed = { ...rule, ...update, version: rule.version + 1 };
    this.db.prepare('UPDATE alert_rules SET definition_json=?,updated_at=? WHERE id=?')
      .run(JSON.stringify(changed), new Date().toISOString(), id);
    return changed;
  }

  listActiveAlerts() {
    return this.db.prepare(`SELECT s.asset_id,a.name AS asset_name,s.rule_id,s.state_json,r.definition_json
      FROM alert_states s JOIN assets a ON a.id=s.asset_id JOIN alert_rules r ON r.id=s.rule_id`).all()
      .map(row => ({ assetId: row.asset_id, assetName: row.asset_name, ruleId: row.rule_id,
        rule: JSON.parse(row.definition_json), state: JSON.parse(row.state_json) }))
      .filter(row => row.state.status === 'firing');
  }

  listAssets() { return this.db.prepare('SELECT * FROM assets ORDER BY created_at DESC').all().map(publicAsset); }
  getAsset(id) { return publicAsset(this.getInternalAsset(id)); }
  getInternalAsset(id) { return this.db.prepare('SELECT * FROM assets WHERE id=?').get(id) ?? null; }

  addAsset(a, encryptedPassword, encryptedPoolerUrl = null) {
    const id = randomUUID();
    const now = new Date().toISOString();
    this.db.prepare(`INSERT INTO assets
      (id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,linux_host,linux_exporter_port,pooler_url_enc,k8s_cluster,k8s_namespace,k8s_workload,k8s_pod,k8s_node,created_at)
      VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`).run(
      id, a.name, a.host, a.port, a.database, a.username, encryptedPassword,
      a.sslMode, a.runtime, a.linuxHost || null, a.linuxExporterPort || null, encryptedPoolerUrl, a.k8sCluster || null,
      a.namespace || null, a.workload || null, a.pod || null, a.node || null, now
    );
    return this.getAsset(id);
  }

  updateAsset(id, a, encryptedPassword, encryptedPoolerUrl = null) {
    const changed = this.db.prepare(`UPDATE assets SET
      name=?,host=?,port=?,database_name=?,username=?,password_enc=?,ssl_mode=?,runtime=?,
      linux_host=?,linux_exporter_port=?,pooler_url_enc=?,k8s_cluster=?,k8s_namespace=?,k8s_workload=?,k8s_pod=?,k8s_node=?,
      last_sample_at=NULL,last_status='pending',last_error=NULL,version_num=NULL,role=NULL
      WHERE id=?`).run(
      a.name, a.host, a.port, a.database, a.username, encryptedPassword, a.sslMode, a.runtime,
      a.linuxHost || null, a.linuxExporterPort || null, encryptedPoolerUrl, a.k8sCluster || null, a.namespace || null,
      a.workload || null, a.pod || null, a.node || null, id
    ).changes;
    return changed ? this.getAsset(id) : null;
  }

  deleteAsset(id) { return this.db.prepare('DELETE FROM assets WHERE id=?').run(id).changes > 0; }

  applyAlerts(assetId, result, previous, error, sampleId, now, onlyRuleId = null) {
    const openByGroup = this.db.prepare("SELECT id FROM events WHERE asset_id=? AND event_group=? AND status='open'");
    const addEvidence = this.db.prepare('INSERT INTO evidence(id,event_id,kind,observed_at,payload_json) VALUES(?,?,?,?,?)');
    const stateFor = this.db.prepare('SELECT state_json FROM alert_states WHERE asset_id=? AND rule_id=?');
    const saveState = this.db.prepare(`INSERT INTO alert_states(asset_id,rule_id,state_json) VALUES(?,?,?)
      ON CONFLICT(asset_id,rule_id) DO UPDATE SET state_json=excluded.state_json`);
    const deleteState = this.db.prepare('DELETE FROM alert_states WHERE asset_id=? AND rule_id=?');
    for (const rule of this.getAlertRules()) {
      if (onlyRuleId && rule.id !== onlyRuleId) continue;
      const previousState = stateFor.get(assetId, rule.id);
      const old = previousState ? JSON.parse(previousState.state_json) : null;
      if (!rule.enabled) {
        if (old?.status === 'firing' && old.eventId) addEvidence.run(randomUUID(), old.eventId, 'alert_disabled', now,
          JSON.stringify({ ruleId: rule.id, ruleVersion: rule.version, sampleId }));
        if (old) deleteState.run(assetId, rule.id);
        continue;
      }
      const signal = readSignal(rule.metric, result, previous, error, Date.parse(now));
      const { next, transition } = nextAlertState(rule, signal, old, now);
      if (transition === 'firing') {
        let eventId = openByGroup.get(assetId, rule.group)?.id;
        if (!eventId) {
          eventId = randomUUID();
          this.db.prepare(`INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
            VALUES(?,?, 'alert_incident', ?, 'open', ?, ?)`).run(eventId, assetId, rule.title, now, rule.group);
        }
        next.eventId = eventId;
      }
      if (transition && (next?.eventId || old?.eventId)) {
        const eventId = next?.eventId ?? old.eventId;
        addEvidence.run(randomUUID(), eventId, `alert_${transition}`, now, JSON.stringify({
          ruleId: rule.id, ruleVersion: rule.version, metric: rule.metric, severity: rule.severity,
          source: signal.source, value: signal.value, threshold: rule.threshold,
          operator: rule.operator, sampleId, detail: signal.detail, reason: signal.reason
        }));
      }
      if (next) saveState.run(assetId, rule.id, JSON.stringify(next));
      else if (old) deleteState.run(assetId, rule.id);
    }
  }

  checkStale(assetId, now = new Date().toISOString()) {
    const asset = this.db.prepare('SELECT created_at,last_sample_at FROM assets WHERE id=?').get(assetId);
    if (!asset) return;
    const interval = Math.max(15, Number(process.env.DBSPHERE_COLLECT_INTERVAL_SECONDS) || 60);
    const last = asset.last_sample_at ?? asset.created_at;
    const missed = Math.max(0, (new Date(now).getTime() - new Date(last).getTime()) / 1000 / interval);
    this.db.exec('BEGIN IMMEDIATE');
    try {
      this.applyAlerts(assetId, { collectorMissedIntervals: missed }, null, null, null, now, 'collector_stale');
      this.db.exec('COMMIT');
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }

  resolveInactiveEvents(assetId, error, result, now) {
    const currentGroup = error && readSignal('pg.unavailable', result, null, error).value === 1 ? 'availability' : 'telemetry';
    const firing = new Set(this.db.prepare("SELECT state_json FROM alert_states WHERE asset_id=?").all(assetId)
      .map(row => JSON.parse(row.state_json)).filter(state => state.status === 'firing').map(state => state.eventId));
    for (const event of this.db.prepare("SELECT id,event_group FROM events WHERE asset_id=? AND status='open' AND kind IN ('collection_failure','alert_incident')").all(assetId)) {
      if ((error && event.event_group === currentGroup) || firing.has(event.id)) continue;
      this.db.prepare("UPDATE events SET status='resolved',ended_at=? WHERE id=?").run(now, event.id);
    }
  }

  recordResult(assetId, result, error = null) {
    const now = new Date().toISOString();
    const status = error ? 'error' : 'ok';
    const sampleId = randomUUID();
    this.db.exec('BEGIN IMMEDIATE');
    try {
      const prior = this.db.prepare('SELECT metrics_json,error FROM samples WHERE asset_id=? ORDER BY collected_at DESC LIMIT 1').get(assetId);
      const previous = prior?.metrics_json ? JSON.parse(prior.metrics_json) : null;
      this.db.prepare('INSERT INTO samples(id,asset_id,collected_at,status,metrics_json,error) VALUES(?,?,?,?,?,?)')
        .run(sampleId, assetId, now, status, result ? JSON.stringify(result) : null, error);
      this.db.prepare(`UPDATE assets SET last_sample_at=?,last_status=?,last_error=?,version_num=COALESCE(?,version_num),role=COALESCE(?,role) WHERE id=?`)
        .run(now, status, error, result?.version_num ?? null, result?.role ?? null, assetId);
      if (error) {
        const group = readSignal('pg.unavailable', result, previous, error).value === 1 ? 'availability' : 'telemetry';
        const open = this.db.prepare("SELECT id FROM events WHERE asset_id=? AND event_group=? AND status='open'").get(assetId, group);
        const eventId = open?.id ?? randomUUID();
        if (!open) this.db.prepare('INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group) VALUES(?,?,?,?,?,?,?)')
          .run(eventId, assetId, 'collection_failure', group === 'availability' ? 'PostgreSQL 无法连接' : 'PostgreSQL 采集失败', 'open', now, group);
        const collectionEvidenceId = randomUUID();
        this.db.prepare('INSERT INTO evidence(id,event_id,kind,observed_at,payload_json) VALUES(?,?,?,?,?)')
          .run(collectionEvidenceId, eventId, 'collection_error', now, JSON.stringify({ sampleId, error }));
        if (result?.k8s) {
          const k8sEvidenceId = randomUUID();
          this.db.prepare('INSERT INTO evidence(id,event_id,kind,observed_at,payload_json) VALUES(?,?,?,?,?)')
            .run(k8sEvidenceId, eventId, 'k8s_state', now, JSON.stringify(result.k8s));
          const candidate = oomCandidate(result, error, collectionEvidenceId, k8sEvidenceId);
          const hasCandidate = this.db.prepare("SELECT 1 FROM evidence WHERE event_id=? AND kind='root_cause_candidate' LIMIT 1").get(eventId);
          if (candidate && !hasCandidate) {
            this.db.prepare('INSERT INTO evidence(id,event_id,kind,observed_at,payload_json) VALUES(?,?,?,?,?)')
              .run(randomUUID(), eventId, 'root_cause_candidate', now, JSON.stringify(candidate));
          }
        }
        if (result?.linux) {
          this.db.prepare('INSERT INTO evidence(id,event_id,kind,observed_at,payload_json) VALUES(?,?,?,?,?)')
            .run(randomUUID(), eventId, 'linux_state', now, JSON.stringify(result.linux));
        }
        if (result?.pgbouncer) {
          this.db.prepare('INSERT INTO evidence(id,event_id,kind,observed_at,payload_json) VALUES(?,?,?,?,?)')
            .run(randomUUID(), eventId, 'pgbouncer_state', now, JSON.stringify(result.pgbouncer));
        }
      } else if (prior?.error) {
        const group = readSignal('pg.unavailable', previous, null, prior.error).value === 1 ? 'availability' : 'telemetry';
        const open = this.db.prepare("SELECT id FROM events WHERE asset_id=? AND event_group=? AND status='open'").get(assetId, group);
        if (open) this.db.prepare('INSERT INTO evidence(id,event_id,kind,observed_at,payload_json) VALUES(?,?,?,?,?)')
          .run(randomUUID(), open.id, 'collection_recovered', now, JSON.stringify({ sampleId }));
      }
      this.applyAlerts(assetId, result, previous, error, sampleId, now);
      this.resolveInactiveEvents(assetId, error, result, now);
      this.db.exec('COMMIT');
    } catch (e) { this.db.exec('ROLLBACK'); throw e; }
    return { sampleId, status, collectedAt: now };
  }

  listSamples(assetId, limit = 100) {
    return this.db.prepare('SELECT * FROM samples WHERE asset_id=? ORDER BY collected_at DESC LIMIT ?').all(assetId, limit)
      .map(r => ({ id: r.id, assetId: r.asset_id, collectedAt: r.collected_at, status: r.status, metrics: r.metrics_json ? JSON.parse(r.metrics_json) : null, error: r.error }));
  }

  listEvents() {
    return this.db.prepare('SELECT e.*, a.name AS asset_name FROM events e JOIN assets a ON a.id=e.asset_id ORDER BY e.started_at DESC LIMIT 200').all()
      .map(r => ({ id: r.id, assetId: r.asset_id, assetName: r.asset_name, kind: r.kind,
        eventGroup: r.event_group, title: r.title, status: r.status, startedAt: r.started_at,
        endedAt: r.ended_at, acknowledgedAt: r.acknowledged_at }));
  }

  acknowledgeEvent(id) {
    const now = new Date().toISOString();
    this.db.exec('BEGIN IMMEDIATE');
    try {
      const event = this.db.prepare('SELECT id,status,acknowledged_at FROM events WHERE id=?').get(id);
      if (!event) { this.db.exec('ROLLBACK'); return null; }
      if (event.status === 'open' && !event.acknowledged_at) {
        this.db.prepare('UPDATE events SET acknowledged_at=? WHERE id=?').run(now, id);
        this.db.prepare('INSERT INTO evidence(id,event_id,kind,observed_at,payload_json) VALUES(?,?,?,?,?)')
          .run(randomUUID(), id, 'event_acknowledged', now, JSON.stringify({ actor: 'admin' }));
      }
      this.db.exec('COMMIT');
      return this.getEvent(id);
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }

  getEvent(id) {
    const r = this.db.prepare('SELECT e.*, a.name AS asset_name FROM events e JOIN assets a ON a.id=e.asset_id WHERE e.id=?').get(id);
    if (!r) return null;
    const evidence = this.db.prepare('SELECT * FROM evidence WHERE event_id=? ORDER BY observed_at').all(id)
      .map(e => ({ id: e.id, kind: e.kind, observedAt: e.observed_at, payload: JSON.parse(e.payload_json) }));
    return { id: r.id, assetId: r.asset_id, assetName: r.asset_name, kind: r.kind,
      eventGroup: r.event_group, title: r.title, status: r.status, startedAt: r.started_at,
      endedAt: r.ended_at, acknowledgedAt: r.acknowledged_at, evidence };
  }

  close() { this.db.close(); }
}
