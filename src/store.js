import { DatabaseSync } from 'node:sqlite';
import { randomUUID } from 'node:crypto';
import { join } from 'node:path';
import { oomCandidate } from './incidents.js';

export const publicAsset = (row) => row && ({
  id: row.id, name: row.name, host: row.host, port: row.port,
  database: row.database_name, username: row.username, sslMode: row.ssl_mode,
  runtime: row.runtime, linuxHost: row.linux_host,
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
        ssl_mode TEXT NOT NULL, runtime TEXT NOT NULL, linux_host TEXT, k8s_cluster TEXT,
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
        started_at TEXT NOT NULL, ended_at TEXT
      );
      CREATE INDEX IF NOT EXISTS events_asset_status ON events(asset_id, status);
      CREATE TABLE IF NOT EXISTS evidence (
        id TEXT PRIMARY KEY, event_id TEXT NOT NULL REFERENCES events(id) ON DELETE CASCADE,
        kind TEXT NOT NULL, observed_at TEXT NOT NULL, payload_json TEXT NOT NULL
      );
      CREATE INDEX IF NOT EXISTS evidence_event_time ON evidence(event_id, observed_at);
    `);
  }

  getSetting(key) { return this.db.prepare('SELECT value FROM settings WHERE key=?').get(key)?.value ?? null; }
  health() { this.db.prepare('SELECT 1').get(); }
  setSetting(key, value) { this.db.prepare('INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value').run(key, value); }

  listAssets() { return this.db.prepare('SELECT * FROM assets ORDER BY created_at DESC').all().map(publicAsset); }
  getAsset(id) { return publicAsset(this.getInternalAsset(id)); }
  getInternalAsset(id) { return this.db.prepare('SELECT * FROM assets WHERE id=?').get(id) ?? null; }

  addAsset(a, encryptedPassword) {
    const id = randomUUID();
    const now = new Date().toISOString();
    this.db.prepare(`INSERT INTO assets
      (id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,linux_host,k8s_cluster,k8s_namespace,k8s_workload,k8s_pod,k8s_node,created_at)
      VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`).run(
      id, a.name, a.host, a.port, a.database, a.username, encryptedPassword,
      a.sslMode, a.runtime, a.linuxHost || null, a.k8sCluster || null,
      a.namespace || null, a.workload || null, a.pod || null, a.node || null, now
    );
    return this.getAsset(id);
  }

  updateAsset(id, a, encryptedPassword) {
    const changed = this.db.prepare(`UPDATE assets SET
      name=?,host=?,port=?,database_name=?,username=?,password_enc=?,ssl_mode=?,runtime=?,
      linux_host=?,k8s_cluster=?,k8s_namespace=?,k8s_workload=?,k8s_pod=?,k8s_node=?,
      last_sample_at=NULL,last_status='pending',last_error=NULL,version_num=NULL,role=NULL
      WHERE id=?`).run(
      a.name, a.host, a.port, a.database, a.username, encryptedPassword, a.sslMode, a.runtime,
      a.linuxHost || null, a.k8sCluster || null, a.namespace || null,
      a.workload || null, a.pod || null, a.node || null, id
    ).changes;
    return changed ? this.getAsset(id) : null;
  }

  deleteAsset(id) { return this.db.prepare('DELETE FROM assets WHERE id=?').run(id).changes > 0; }

  recordResult(assetId, result, error = null) {
    const now = new Date().toISOString();
    const status = error ? 'error' : 'ok';
    const sampleId = randomUUID();
    this.db.exec('BEGIN IMMEDIATE');
    try {
      this.db.prepare('INSERT INTO samples(id,asset_id,collected_at,status,metrics_json,error) VALUES(?,?,?,?,?,?)')
        .run(sampleId, assetId, now, status, result ? JSON.stringify(result) : null, error);
      this.db.prepare(`UPDATE assets SET last_sample_at=?,last_status=?,last_error=?,version_num=COALESCE(?,version_num),role=COALESCE(?,role) WHERE id=?`)
        .run(now, status, error, result?.version_num ?? null, result?.role ?? null, assetId);
      const open = this.db.prepare("SELECT id FROM events WHERE asset_id=? AND kind='collection_failure' AND status='open'").get(assetId);
      if (error) {
        const eventId = open?.id ?? randomUUID();
        if (!open) this.db.prepare('INSERT INTO events(id,asset_id,kind,title,status,started_at) VALUES(?,?,?,?,?,?)')
          .run(eventId, assetId, 'collection_failure', 'PostgreSQL 采集连接失败', 'open', now);
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
      } else if (open) {
        this.db.prepare("UPDATE events SET status='resolved',ended_at=? WHERE id=?").run(now, open.id);
        this.db.prepare('INSERT INTO evidence(id,event_id,kind,observed_at,payload_json) VALUES(?,?,?,?,?)')
          .run(randomUUID(), open.id, 'collection_recovered', now, JSON.stringify({ sampleId }));
      }
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
      .map(r => ({ id: r.id, assetId: r.asset_id, assetName: r.asset_name, kind: r.kind, title: r.title, status: r.status, startedAt: r.started_at, endedAt: r.ended_at }));
  }

  getEvent(id) {
    const r = this.db.prepare('SELECT e.*, a.name AS asset_name FROM events e JOIN assets a ON a.id=e.asset_id WHERE e.id=?').get(id);
    if (!r) return null;
    const evidence = this.db.prepare('SELECT * FROM evidence WHERE event_id=? ORDER BY observed_at').all(id)
      .map(e => ({ id: e.id, kind: e.kind, observedAt: e.observed_at, payload: JSON.parse(e.payload_json) }));
    return { id: r.id, assetId: r.asset_id, assetName: r.asset_name, kind: r.kind, title: r.title, status: r.status, startedAt: r.started_at, endedAt: r.ended_at, evidence };
  }

  close() { this.db.close(); }
}
