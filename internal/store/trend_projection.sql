CREATE TABLE monitoring_trend_points (
  asset_id uuid NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
  collected_at timestamptz NOT NULL,
  id uuid NOT NULL,
  status text NOT NULL,
  pg_status text,
  pg_role text,
  wal_reset text,
  server_started text,
  wal_timeline text,
  xid_used numeric,
  linux_cpu numeric,
  linux_memory numeric,
  connections numeric,
  max_connections numeric,
  database_bytes numeric,
  replication_lag_bytes numeric,
  blocked_sessions numeric,
  long_transactions numeric,
  idle_in_transaction numeric,
  deadlocks numeric,
  commits numeric,
  rollbacks numeric,
  wal_bytes numeric,
  PRIMARY KEY (asset_id, collected_at, id)
);
CREATE INDEX monitoring_trend_points_time ON monitoring_trend_points(collected_at);
CREATE TABLE monitoring_trend_progress (
  singleton boolean PRIMARY KEY CHECK (singleton),
  cursor timestamptz NOT NULL
);
-- The backfill starts from the upper edge of the current UTC hour.
INSERT INTO monitoring_trend_progress VALUES (true,date_trunc('hour',now(),'UTC')+interval '1 hour');

CREATE FUNCTION dbsphere_trend_fields(sample_status text, source_json jsonb)
RETURNS TABLE (
  pg_status text,
  pg_role text,
  wal_reset text,
  server_started text,
  wal_timeline text,
  xid_used numeric,
  linux_cpu numeric,
  linux_memory numeric,
  connections numeric,
  max_connections numeric,
  database_bytes numeric,
  replication_lag_bytes numeric,
  blocked_sessions numeric,
  long_transactions numeric,
  idle_in_transaction numeric,
  deadlocks numeric,
  commits numeric,
  rollbacks numeric,
  wal_bytes numeric
) LANGUAGE sql IMMUTABLE AS $$
  SELECT
    m.collector#>>'{pgStatus}' AS pg_status,
    CASE WHEN sample_status='ok' THEN m.role#>>'{}' END AS pg_role,
    m.diagnostics#>>'{wal,data,statsReset}' AS wal_reset,
    m.diagnostics#>>'{walPosition,data,startedAt}' AS server_started,
    m.diagnostics#>>'{walPosition,data,timeline}' AS wal_timeline,
    CASE WHEN jsonb_typeof(m.diagnostics#>'{freeze,data,usedPercent}')='number'
    	THEN (m.diagnostics#>>'{freeze,data,usedPercent}')::numeric END AS xid_used,
    CASE WHEN m.linux#>>'{status}'='ok' AND jsonb_typeof(m.linux#>'{cpu,usedPercent}')='number'
    	THEN (m.linux#>>'{cpu,usedPercent}')::numeric END AS linux_cpu,
    CASE WHEN m.linux#>>'{status}'='ok' AND jsonb_typeof(m.linux#>'{memory,usedPercent}')='number'
    	THEN (m.linux#>>'{memory,usedPercent}')::numeric END AS linux_memory,
    CASE WHEN sample_status='ok' AND jsonb_typeof(m.connections)='number'
    	THEN (m.connections#>>'{}')::numeric END AS connections,
    CASE WHEN sample_status='ok' AND jsonb_typeof(m.max_connections)='number'
    	THEN (m.max_connections#>>'{}')::numeric END AS max_connections,
    CASE WHEN sample_status='ok' AND jsonb_typeof(m.database_bytes)='number'
    	THEN (m.database_bytes#>>'{}')::numeric END AS database_bytes,
    CASE WHEN sample_status='ok' AND jsonb_typeof(m.replication_lag_bytes)='number'
    	THEN (m.replication_lag_bytes#>>'{}')::numeric END AS replication_lag_bytes,
    CASE WHEN sample_status='ok' AND jsonb_typeof(m.diagnostics#>'{locks,data,blockedSessions}')='number'
    	THEN (m.diagnostics#>>'{locks,data,blockedSessions}')::numeric END AS blocked_sessions,
    CASE WHEN sample_status='ok' AND jsonb_typeof(m.diagnostics#>'{transactions,data,longTransactions}')='number'
    	THEN (m.diagnostics#>>'{transactions,data,longTransactions}')::numeric END AS long_transactions,
    CASE WHEN m.diagnostics#>>'{transactions,status}'='ok'
    	AND jsonb_typeof(m.diagnostics#>'{transactions,data,idleInTransaction}')='number'
    	THEN (m.diagnostics#>>'{transactions,data,idleInTransaction}')::numeric END AS idle_in_transaction,
    CASE WHEN m.diagnostics#>>'{database,status}'='ok'
    	AND jsonb_typeof(m.diagnostics#>'{database,data,deadlocks}')='number'
    	THEN (m.diagnostics#>>'{database,data,deadlocks}')::numeric END AS deadlocks,
    CASE WHEN sample_status='ok' AND jsonb_typeof(m.xact_commit)='number'
    	THEN (m.xact_commit#>>'{}')::numeric END AS commits,
    CASE WHEN sample_status='ok' AND jsonb_typeof(m.xact_rollback)='number'
    	THEN (m.xact_rollback#>>'{}')::numeric END AS rollbacks,
    CASE WHEN sample_status='ok' AND (m.diagnostics#>>'{wal,data,bytes}') ~ '^[0-9]+([.][0-9]+)?$'
    	THEN (m.diagnostics#>>'{wal,data,bytes}')::numeric END AS wal_bytes
  FROM jsonb_to_record(CASE WHEN jsonb_typeof(source_json)='object' THEN source_json ELSE '{}'::jsonb END)
    AS m(collector jsonb,
      diagnostics jsonb,
      linux jsonb,
      role jsonb,
      connections jsonb,
      max_connections jsonb,
      database_bytes jsonb,
      replication_lag_bytes jsonb,
      xact_commit jsonb,
      xact_rollback jsonb)
$$;

CREATE FUNCTION dbsphere_sync_trend_sample()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF TG_OP IN ('DELETE','UPDATE') THEN
    DELETE FROM monitoring_trend_points WHERE asset_id=OLD.asset_id AND collected_at=OLD.collected_at AND id=OLD.id;
  END IF;
  IF TG_OP IN ('INSERT','UPDATE') AND NEW.collected_at >= date_trunc('hour',now(),'UTC')-interval '31 days' THEN
    INSERT INTO monitoring_trend_points
      SELECT NEW.asset_id,NEW.collected_at,NEW.id,NEW.status,f.* FROM dbsphere_trend_fields(NEW.status,NEW.metrics_json) f
      ON CONFLICT DO NOTHING;
  END IF;
  RETURN NULL;
END;
$$;
CREATE TRIGGER monitoring_trend_sample_change
  AFTER INSERT OR UPDATE OR DELETE ON samples
  FOR EACH ROW EXECUTE FUNCTION dbsphere_sync_trend_sample();
