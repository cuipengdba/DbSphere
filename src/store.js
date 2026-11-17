import { DatabaseSync } from 'node:sqlite';
import { randomUUID } from 'node:crypto';
import { join } from 'node:path';
import { oomCandidate } from './incidents.js';
import { DEFAULT_RULES, readSignal, validateRuleUpdate } from './alert-rules.js';
import { nextAlertState } from './alert-state.js';
import { DEFAULT_INSPECTION_TEMPLATES, upgradeInspectionTemplate, validateInspectionTemplateUpdate } from './inspections.js';

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

export const publicSilence = row => row && ({ id: row.id, kind: row.kind, assetId: row.asset_id,
  ruleId: row.rule_id, startsAt: row.starts_at, endsAt: row.ends_at,
  reason: row.reason, createdAt: row.created_at, revokedAt: row.revoked_at });

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
      CREATE TABLE IF NOT EXISTS silences (
        id TEXT PRIMARY KEY, kind TEXT NOT NULL, asset_id TEXT REFERENCES assets(id) ON DELETE CASCADE,
        rule_id TEXT REFERENCES alert_rules(id), starts_at TEXT NOT NULL, ends_at TEXT NOT NULL,
        reason TEXT NOT NULL, created_at TEXT NOT NULL, revoked_at TEXT
      );
      CREATE INDEX IF NOT EXISTS silences_active ON silences(starts_at,ends_at);
      CREATE TABLE IF NOT EXISTS notification_outbox (
        id TEXT PRIMARY KEY, evidence_id TEXT NOT NULL REFERENCES evidence(id) ON DELETE CASCADE,
        channel TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending',
        attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at TEXT NOT NULL,
        delivered_at TEXT, last_error TEXT, UNIQUE(evidence_id,channel)
      );
      CREATE INDEX IF NOT EXISTS notification_outbox_due ON notification_outbox(status,next_attempt_at);
      CREATE TABLE IF NOT EXISTS inspection_templates (
        id TEXT PRIMARY KEY, definition_json TEXT NOT NULL, updated_at TEXT NOT NULL
      );
      CREATE TABLE IF NOT EXISTS inspection_schedules (
        asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
        template_id TEXT NOT NULL REFERENCES inspection_templates(id),
        interval_minutes INTEGER NOT NULL, next_due_at TEXT NOT NULL, enabled INTEGER NOT NULL DEFAULT 1,
        PRIMARY KEY(asset_id,template_id)
      );
      CREATE TABLE IF NOT EXISTS inspection_runs (
        id TEXT PRIMARY KEY, asset_id TEXT NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
        template_id TEXT NOT NULL REFERENCES inspection_templates(id), template_version INTEGER NOT NULL,
        sample_id TEXT REFERENCES samples(id) ON DELETE SET NULL, retry_of TEXT,
        started_at TEXT NOT NULL, completed_at TEXT NOT NULL,
        summary_json TEXT NOT NULL, findings_json TEXT NOT NULL,
        status TEXT NOT NULL DEFAULT 'completed', error TEXT,
        event_ids_json TEXT NOT NULL DEFAULT '[]'
      );
      CREATE INDEX IF NOT EXISTS inspection_runs_asset_time ON inspection_runs(asset_id,completed_at DESC);
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
    if (!this.db.prepare('PRAGMA table_info(inspection_runs)').all().some(column => column.name === 'status')) {
      this.db.exec("ALTER TABLE inspection_runs ADD COLUMN status TEXT NOT NULL DEFAULT 'completed'");
    }
    if (!this.db.prepare('PRAGMA table_info(inspection_runs)').all().some(column => column.name === 'error')) {
      this.db.exec('ALTER TABLE inspection_runs ADD COLUMN error TEXT');
    }
    if (!this.db.prepare('PRAGMA table_info(inspection_runs)').all().some(column => column.name === 'event_ids_json')) {
      this.db.exec("ALTER TABLE inspection_runs ADD COLUMN event_ids_json TEXT NOT NULL DEFAULT '[]'");
    }
    this.db.exec("UPDATE events SET event_group='telemetry' WHERE event_group IS NULL");
    this.db.exec("CREATE UNIQUE INDEX IF NOT EXISTS events_one_open_group ON events(asset_id,event_group) WHERE status='open'");
    const insertRule = this.db.prepare('INSERT OR IGNORE INTO alert_rules(id,definition_json,updated_at) VALUES(?,?,?)');
    for (const rule of DEFAULT_RULES) insertRule.run(rule.id, JSON.stringify(rule), new Date().toISOString());
    const insertTemplate = this.db.prepare('INSERT OR IGNORE INTO inspection_templates(id,definition_json,updated_at) VALUES(?,?,?)');
    for (const template of DEFAULT_INSPECTION_TEMPLATES) {
      insertTemplate.run(template.id, JSON.stringify(template), new Date().toISOString());
      const upgraded = upgradeInspectionTemplate(this.getInspectionTemplate(template.id), template);
      if (upgraded) this.db.prepare('UPDATE inspection_templates SET definition_json=?,updated_at=? WHERE id=?')
        .run(JSON.stringify(upgraded), new Date().toISOString(), template.id);
    }
  }

  getSetting(key) { return this.db.prepare('SELECT value FROM settings WHERE key=?').get(key)?.value ?? null; }
  health() { this.db.prepare('SELECT 1').get(); }
  setSetting(key, value) { this.db.prepare('INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value').run(key, value); }

  getInspectionTemplates() {
    return this.db.prepare('SELECT definition_json FROM inspection_templates ORDER BY id').all()
      .map(row => JSON.parse(row.definition_json));
  }
  getInspectionTemplate(id) {
    const row = this.db.prepare('SELECT definition_json FROM inspection_templates WHERE id=?').get(id);
    return row ? JSON.parse(row.definition_json) : null;
  }
  updateInspectionTemplate(id, input) {
    const updated = validateInspectionTemplateUpdate(this.getInspectionTemplate(id), input);
    if (!updated) return null;
    this.db.prepare('UPDATE inspection_templates SET definition_json=?,updated_at=? WHERE id=?')
      .run(JSON.stringify(updated), new Date().toISOString(), id);
    return updated;
  }
  listInspectionSchedules() {
    return this.db.prepare('SELECT * FROM inspection_schedules ORDER BY next_due_at').all()
      .map(row => ({ assetId: row.asset_id, templateId: row.template_id,
        intervalMinutes: row.interval_minutes, nextDueAt: row.next_due_at, enabled: Boolean(row.enabled) }));
  }
  upsertInspectionSchedule(assetId, templateId, intervalMinutes, enabled = true) {
    if (!this.getInternalAsset(assetId) || !this.getInspectionTemplate(templateId)) return null;
    const nextDueAt = new Date(Date.now() + intervalMinutes * 60000).toISOString();
    this.db.prepare(`INSERT INTO inspection_schedules(asset_id,template_id,interval_minutes,next_due_at,enabled)
      VALUES(?,?,?,?,?) ON CONFLICT(asset_id,template_id) DO UPDATE SET interval_minutes=excluded.interval_minutes,
      next_due_at=excluded.next_due_at,enabled=excluded.enabled`).run(assetId, templateId, intervalMinutes, nextDueAt, enabled ? 1 : 0);
    return { assetId, templateId, intervalMinutes, nextDueAt, enabled };
  }
  claimDueInspections(now = new Date().toISOString(), limit = 20) {
    this.db.exec('BEGIN IMMEDIATE');
    try {
      const rows = this.db.prepare(`SELECT * FROM inspection_schedules WHERE enabled=1 AND next_due_at<=?
        ORDER BY next_due_at LIMIT ?`).all(now, limit);
      for (const row of rows) this.db.prepare('UPDATE inspection_schedules SET next_due_at=? WHERE asset_id=? AND template_id=?')
        .run(new Date(new Date(now).getTime() + row.interval_minutes * 60000).toISOString(), row.asset_id, row.template_id);
      this.db.exec('COMMIT');
      return rows.map(row => ({ assetId: row.asset_id, templateId: row.template_id }));
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
  }
  saveInspectionRun(run) {
    this.db.prepare(`INSERT INTO inspection_runs(id,asset_id,template_id,template_version,sample_id,retry_of,
      started_at,completed_at,summary_json,findings_json,status,error,event_ids_json) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
      .run(run.id, run.assetId, run.templateId, run.templateVersion, run.sampleId, run.retryOf,
        run.startedAt, run.completedAt, JSON.stringify(run.summary), JSON.stringify(run.findings),
        run.status ?? 'completed', run.error ?? null, JSON.stringify(run.eventIds ?? []));
    return run;
  }
  listInspectionRuns(assetId = null, limit = 50) {
    const rows = assetId
      ? this.db.prepare('SELECT * FROM inspection_runs WHERE asset_id=? ORDER BY completed_at DESC LIMIT ?').all(assetId, limit)
      : this.db.prepare('SELECT * FROM inspection_runs ORDER BY completed_at DESC LIMIT ?').all(limit);
    return rows.map(row => ({ id: row.id, assetId: row.asset_id, templateId: row.template_id,
      templateVersion: row.template_version, sampleId: row.sample_id, retryOf: row.retry_of,
      startedAt: row.started_at, completedAt: row.completed_at, status: row.status, error: row.error,
      eventIds: JSON.parse(row.event_ids_json),
      summary: JSON.parse(row.summary_json), findings: JSON.parse(row.findings_json) }));
  }
  getInspectionRun(id) {
    const row = this.db.prepare('SELECT * FROM inspection_runs WHERE id=?').get(id);
    return row ? { id: row.id, assetId: row.asset_id, templateId: row.template_id,
      templateVersion: row.template_version, sampleId: row.sample_id, retryOf: row.retry_of,
      startedAt: row.started_at, completedAt: row.completed_at, status: row.status, error: row.error,
      eventIds: JSON.parse(row.event_ids_json),
      summary: JSON.parse(row.summary_json), findings: JSON.parse(row.findings_json) } : null;
  }

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

  listSilences() {
    return this.db.prepare('SELECT * FROM silences ORDER BY created_at DESC LIMIT 200').all().map(publicSilence);
  }

  createSilence(input) {
    if (input.assetId && !this.getInternalAsset(input.assetId)) return null;
    if (input.ruleId && !this.db.prepare('SELECT 1 FROM alert_rules WHERE id=?').get(input.ruleId)) return null;
    const id = randomUUID();
    const createdAt = new Date().toISOString();
    this.db.prepare(`INSERT INTO silences(id,kind,asset_id,rule_id,starts_at,ends_at,reason,created_at)
      VALUES(?,?,?,?,?,?,?,?)`).run(id, input.kind, input.assetId, input.ruleId,
      input.startsAt, input.endsAt, input.reason, createdAt);
    return publicSilence(this.db.prepare('SELECT * FROM silences WHERE id=?').get(id));
  }

  revokeSilence(id) {
    const now = new Date().toISOString();
    const changed = this.db.prepare('UPDATE silences SET revoked_at=? WHERE id=? AND revoked_at IS NULL').run(now, id).changes;
    return changed ? publicSilence(this.db.prepare('SELECT * FROM silences WHERE id=?').get(id)) : null;
  }

  listDueNotifications(now = new Date().toISOString(), limit = 50) {
    return this.db.prepare(`SELECT n.id,n.channel,n.attempts,e.id AS evidence_id,e.kind,e.observed_at,
      e.payload_json,v.id AS event_id,v.title,v.status AS event_status,a.id AS asset_id,a.name AS asset_name
      FROM notification_outbox n JOIN evidence e ON e.id=n.evidence_id
      JOIN events v ON v.id=e.event_id JOIN assets a ON a.id=v.asset_id
      WHERE n.status='pending' AND n.next_attempt_at<=?
      ORDER BY n.next_attempt_at,n.id LIMIT ?`).all(now, limit).map(row => ({
      id: row.id, channel: row.channel, attempts: row.attempts, evidenceId: row.evidence_id,
      kind: row.kind, observedAt: row.observed_at, payload: JSON.parse(row.payload_json),
      eventId: row.event_id, eventTitle: row.title, eventStatus: row.event_status,
      assetId: row.asset_id, assetName: row.asset_name
    }));
  }

  listNotificationHistory(limit = 100) {
    return this.db.prepare(`SELECT n.id,n.channel,n.status,n.attempts,n.next_attempt_at,n.delivered_at,
      n.last_error,e.kind,e.observed_at,v.id AS event_id,a.name AS asset_name
      FROM notification_outbox n JOIN evidence e ON e.id=n.evidence_id
      JOIN events v ON v.id=e.event_id JOIN assets a ON a.id=v.asset_id
      ORDER BY e.observed_at DESC,n.id DESC LIMIT ?`).all(limit).map(row => ({
      id: row.id, channel: row.channel, status: row.status, attempts: row.attempts,
      nextAttemptAt: row.next_attempt_at, deliveredAt: row.delivered_at,
      lastError: row.last_error, kind: row.kind, observedAt: row.observed_at,
      eventId: row.event_id, assetName: row.asset_name
    }));
  }

  completeNotification(id, outcome, now = new Date().toISOString()) {
    if (outcome === 'delivered' || outcome === 'skipped') {
      return this.db.prepare(`UPDATE notification_outbox SET status=?,delivered_at=?
        WHERE id=? AND status='pending'`).run(outcome, now, id).changes > 0;
    }
    const row = this.db.prepare('SELECT attempts FROM notification_outbox WHERE id=? AND status=?').get(id, 'pending');
    if (!row) return false;
    const attempts = row.attempts + 1;
    const next = new Date(Date.parse(now) + Math.min(3600, 30 * 2 ** (attempts - 1)) * 1000).toISOString();
    return this.db.prepare(`UPDATE notification_outbox SET status=?,attempts=?,next_attempt_at=?,last_error=?
      WHERE id=? AND status='pending'`).run(attempts >= 5 ? 'failed' : 'pending',
      attempts, next, String(outcome).slice(0, 300), id).changes > 0;
  }

  retryNotification(id, now = new Date().toISOString()) {
    return this.db.prepare(`UPDATE notification_outbox SET status='pending',attempts=0,
      next_attempt_at=?,last_error=NULL WHERE id=? AND status='failed'`).run(now, id).changes > 0;
  }

  queueEscalations(now = new Date().toISOString(), afterMinutes = 15) {
    const cutoff = new Date(Date.parse(now) - afterMinutes * 60000).toISOString();
    let queued = 0;
    this.db.exec('BEGIN IMMEDIATE');
    try {
      for (const alert of this.listActiveAlerts()) {
        if (alert.rule.severity !== 'critical' || alert.state.mutedBy) continue;
        const event = this.db.prepare(`SELECT id FROM events WHERE id=? AND status='open'
          AND acknowledged_at IS NULL AND started_at<=?`).get(alert.state.eventId, cutoff);
        if (!event) continue;
        const silenced = this.db.prepare(`SELECT 1 FROM silences WHERE revoked_at IS NULL
          AND starts_at<=? AND ends_at>? AND (asset_id IS NULL OR asset_id=?)
          AND (rule_id IS NULL OR rule_id=?) LIMIT 1`).get(now, now, alert.assetId, alert.ruleId);
        if (silenced) continue;
        const existing = this.db.prepare(`SELECT 1 FROM evidence WHERE event_id=? AND kind='alert_escalation'
          AND json_extract(payload_json,'$.ruleId')=? LIMIT 1`).get(event.id, alert.ruleId);
        if (existing) continue;
        const evidenceId = randomUUID();
        this.db.prepare('INSERT INTO evidence(id,event_id,kind,observed_at,payload_json) VALUES(?,?,?,?,?)')
          .run(evidenceId, event.id, 'alert_escalation', now, JSON.stringify({ ruleId: alert.ruleId,
            severity: alert.rule.severity, source: alert.rule.source, value: alert.state.lastValue,
            afterMinutes }));
        for (const channel of ['webhook', 'email']) this.db.prepare(`INSERT INTO notification_outbox
          (id,evidence_id,channel,next_attempt_at) VALUES(?,?,?,?)`).run(randomUUID(), evidenceId, channel, now);
        queued++;
      }
      this.db.exec('COMMIT');
      return queued;
    } catch (error) { this.db.exec('ROLLBACK'); throw error; }
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
    const queueNotification = this.db.prepare(`INSERT INTO notification_outbox(id,evidence_id,channel,next_attempt_at)
      VALUES(?,?,?,?)`);
    const silences = this.db.prepare(`SELECT id,rule_id FROM silences WHERE revoked_at IS NULL
      AND starts_at<=? AND ends_at>? AND (asset_id IS NULL OR asset_id=?)
      ORDER BY created_at DESC`).all(now, now, assetId);
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
      if (next) next.mutedBy = silences.find(item => !item.rule_id || item.rule_id === rule.id)?.id ?? null;
      if (transition === 'firing') {
        let eventId = openByGroup.get(assetId, rule.group)?.id;
        if (!eventId) {
          eventId = randomUUID();
          this.db.prepare(`INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
            VALUES(?,?, 'alert_incident', ?, 'open', ?, ?)`).run(eventId, assetId, rule.title, now, rule.group);
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
        const eventId = next?.eventId ?? old.eventId;
        const evidenceId = randomUUID();
        addEvidence.run(evidenceId, eventId, `alert_${change}`, now, JSON.stringify({
          ruleId: rule.id, ruleVersion: rule.version, metric: rule.metric, severity: rule.severity,
          source: signal.source, value: signal.value, threshold: rule.threshold,
          operator: rule.operator, sampleId, detail: signal.detail, reason: signal.reason,
          silenceId: next?.mutedBy ?? old?.mutedBy ?? null
        }));
        if (['firing', 'unsuppressed', 'recovered'].includes(change) &&
          !(change === 'recovered' && old?.mutedBy)) {
          for (const channel of ['webhook', 'email']) queueNotification.run(randomUUID(), evidenceId, channel, now);
        }
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
