import pg from 'pg';
import { randomUUID } from 'node:crypto';
import { oomCandidate } from './incidents.js';
import { publicAsset } from './store.js';

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
    ssl_mode text NOT NULL, runtime text NOT NULL, linux_host text, k8s_cluster text,
    k8s_namespace text, k8s_workload text, k8s_pod text, k8s_node text,
    created_at timestamptz NOT NULL, last_sample_at timestamptz,
    last_status text NOT NULL DEFAULT 'pending', last_error text, version_num integer, role text
  );
  CREATE TABLE IF NOT EXISTS samples (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    collected_at timestamptz NOT NULL, status text NOT NULL,
    metrics_json jsonb, error text
  );
  CREATE INDEX IF NOT EXISTS samples_asset_time ON samples(asset_id, collected_at DESC);
  CREATE TABLE IF NOT EXISTS events (
    id uuid PRIMARY KEY, asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    kind text NOT NULL, title text NOT NULL, status text NOT NULL,
    started_at timestamptz NOT NULL, ended_at timestamptz
  );
  CREATE INDEX IF NOT EXISTS events_asset_status ON events(asset_id, status);
  CREATE UNIQUE INDEX IF NOT EXISTS events_one_open_kind ON events(asset_id, kind) WHERE status='open';
  CREATE TABLE IF NOT EXISTS evidence (
    id uuid PRIMARY KEY, event_id uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    kind text NOT NULL, observed_at timestamptz NOT NULL, payload_json jsonb NOT NULL
  );
  CREATE INDEX IF NOT EXISTS evidence_event_time ON evidence(event_id, observed_at);
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

  async listAssets() {
    return (await this.pool.query('SELECT * FROM assets ORDER BY created_at DESC')).rows.map(publicAsset);
  }
  async getInternalAsset(id) {
    return (await this.pool.query('SELECT * FROM assets WHERE id=$1', [id])).rows[0] ?? null;
  }
  async getAsset(id) { return publicAsset(await this.getInternalAsset(id)); }

  async addAsset(a, encryptedPassword) {
    const id = randomUUID();
    await this.pool.query(`INSERT INTO assets
      (id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,linux_host,k8s_cluster,k8s_namespace,k8s_workload,k8s_pod,k8s_node,created_at)
      VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`, [
      id, a.name, a.host, a.port, a.database, a.username, encryptedPassword,
      a.sslMode, a.runtime, a.linuxHost || null, a.k8sCluster || null,
      a.namespace || null, a.workload || null, a.pod || null, a.node || null,
      new Date().toISOString()
    ]);
    return this.getAsset(id);
  }

  async updateAsset(id, a, encryptedPassword) {
    const updated = await this.pool.query(`UPDATE assets SET
      name=$1,host=$2,port=$3,database_name=$4,username=$5,password_enc=$6,ssl_mode=$7,runtime=$8,
      linux_host=$9,k8s_cluster=$10,k8s_namespace=$11,k8s_workload=$12,k8s_pod=$13,k8s_node=$14,
      last_sample_at=NULL,last_status='pending',last_error=NULL,version_num=NULL,role=NULL
      WHERE id=$15 RETURNING id`, [
      a.name, a.host, a.port, a.database, a.username, encryptedPassword, a.sslMode, a.runtime,
      a.linuxHost || null, a.k8sCluster || null, a.namespace || null,
      a.workload || null, a.pod || null, a.node || null, id
    ]);
    return updated.rowCount ? this.getAsset(id) : null;
  }

  async deleteAsset(id) {
    return (await this.pool.query('DELETE FROM assets WHERE id=$1', [id])).rowCount > 0;
  }

  async recordResult(assetId, result, error = null) {
    const now = new Date().toISOString();
    const status = error ? 'error' : 'ok';
    const sampleId = randomUUID();
    const client = await this.pool.connect();
    try {
      await client.query('BEGIN');
      await client.query(`INSERT INTO samples(id,asset_id,collected_at,status,metrics_json,error)
        VALUES($1,$2,$3,$4,$5,$6)`, [sampleId, assetId, now, status, JSON.stringify(result), error]);
      await client.query(`UPDATE assets SET last_sample_at=$1,last_status=$2,last_error=$3,
        version_num=COALESCE($4,version_num),role=COALESCE($5,role) WHERE id=$6`,
      [now, status, error, result?.version_num ?? null, result?.role ?? null, assetId]);
      const open = (await client.query(`SELECT id FROM events WHERE asset_id=$1 AND kind='collection_failure'
        AND status='open' FOR UPDATE`, [assetId])).rows[0];
      if (error) {
        const eventId = open?.id ?? randomUUID();
        if (!open) await client.query(`INSERT INTO events(id,asset_id,kind,title,status,started_at)
          VALUES($1,$2,'collection_failure','PostgreSQL 采集连接失败','open',$3)`, [eventId, assetId, now]);
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
      } else if (open) {
        await client.query("UPDATE events SET status='resolved',ended_at=$1 WHERE id=$2", [now, open.id]);
        await client.query(`INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
          VALUES($1,$2,'collection_recovered',$3,$4)`, [randomUUID(), open.id, now, JSON.stringify({ sampleId })]);
      }
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
      kind: r.kind, title: r.title, status: r.status, startedAt: r.started_at, endedAt: r.ended_at }));
  }

  async getEvent(id) {
    const row = (await this.pool.query(`SELECT e.*, a.name AS asset_name FROM events e
      JOIN assets a ON a.id=e.asset_id WHERE e.id=$1`, [id])).rows[0];
    if (!row) return null;
    const evidence = (await this.pool.query('SELECT * FROM evidence WHERE event_id=$1 ORDER BY observed_at', [id])).rows
      .map(e => ({ id: e.id, kind: e.kind, observedAt: e.observed_at, payload: e.payload_json }));
    return { id: row.id, assetId: row.asset_id, assetName: row.asset_name,
      kind: row.kind, title: row.title, status: row.status,
      startedAt: row.started_at, endedAt: row.ended_at, evidence };
  }

  async close() { await this.pool.end(); }
}
