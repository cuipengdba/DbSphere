const number = value => value == null ? null : Number(value);

async function optional(client, sql, map) {
  try { return { status: 'ok', data: map((await client.query(sql)).rows) }; }
  catch (error) { return { status: 'unavailable', reason: String(error.code || error.message).slice(0, 120) }; }
}

export async function collectDiagnostics(client, major, inRecovery = false) {
  const activity = await optional(client, `SELECT COALESCE(state,'unknown') AS state,
    COALESCE(wait_event_type,'none') AS wait_type, count(*)::int AS sessions
    FROM pg_stat_activity WHERE datname=current_database() GROUP BY 1,2`, rows => rows.map(r => ({
    state: r.state, waitType: r.wait_type, sessions: number(r.sessions)
  })));
  const transactions = await optional(client, `SELECT
    count(*) FILTER (WHERE xact_start < now() - interval '5 minutes')::int AS long_transactions,
    count(*) FILTER (WHERE state = 'idle in transaction')::int AS idle_in_transaction
    FROM pg_stat_activity WHERE datname=current_database() AND pid <> pg_backend_pid()`, rows => ({
    longTransactions: number(rows[0].long_transactions), idleInTransaction: number(rows[0].idle_in_transaction)
  }));
  const locks = await optional(client, `SELECT count(*)::int AS blocked_sessions
    FROM pg_stat_activity a WHERE a.datname=current_database()
    AND a.pid <> pg_backend_pid() AND cardinality(pg_blocking_pids(a.pid)) > 0`, rows => ({
    blockedSessions: number(rows[0].blocked_sessions)
  }));
  const lockChain = await optional(client, `SELECT a.pid AS waiting_pid,
    b.blocker_pid, a.wait_event_type, a.wait_event,
    a.xact_start AS waiting_since, blocker.xact_start AS blocker_xact_start
    FROM pg_stat_activity a
    CROSS JOIN LATERAL unnest(pg_blocking_pids(a.pid)) AS b(blocker_pid)
    LEFT JOIN pg_stat_activity blocker ON blocker.pid=b.blocker_pid
    WHERE a.datname=current_database() LIMIT 20`, rows => rows.map(r => ({
    waitingPid: number(r.waiting_pid), blockerPid: number(r.blocker_pid),
    waitType: r.wait_event_type, waitEvent: r.wait_event,
    waitingSince: r.waiting_since, blockerXactStart: r.blocker_xact_start
  })));
  const database = await optional(client, `SELECT deadlocks, blks_read, blks_hit, temp_bytes
    FROM pg_stat_database WHERE datname=current_database()`, rows => ({
    deadlocks: number(rows[0].deadlocks), blocksRead: number(rows[0].blks_read),
    blocksHit: number(rows[0].blks_hit), tempBytes: number(rows[0].temp_bytes)
  }));
  const wal = await optional(client, `SELECT wal_bytes::text AS wal_bytes, wal_records,
    wal_fpi, wal_buffers_full FROM pg_stat_wal`, rows => ({
    bytes: rows[0].wal_bytes, records: number(rows[0].wal_records),
    fullPageImages: number(rows[0].wal_fpi), buffersFull: number(rows[0].wal_buffers_full)
  }));
  const archiver = await optional(client, `SELECT archived_count, failed_count,
    last_archived_time, last_failed_time FROM pg_stat_archiver`, rows => ({
    archivedCount: number(rows[0].archived_count), failedCount: number(rows[0].failed_count),
    lastArchivedAt: rows[0].last_archived_time, lastFailedAt: rows[0].last_failed_time
  }));
  const checkpointSql = major >= 17
    ? 'SELECT num_timed AS timed, num_requested AS requested FROM pg_stat_checkpointer'
    : 'SELECT checkpoints_timed AS timed, checkpoints_req AS requested FROM pg_stat_bgwriter';
  const checkpoints = await optional(client, checkpointSql, rows => ({
    timed: number(rows[0].timed), requested: number(rows[0].requested)
  }));
  const vacuum = await optional(client, `SELECT schemaname, relname, n_live_tup,
    n_dead_tup, last_autovacuum, autovacuum_count
    FROM pg_stat_user_tables ORDER BY n_dead_tup DESC LIMIT 10`, rows => rows.map(r => ({
    schema: r.schemaname, table: r.relname,
    liveRowsEstimate: number(r.n_live_tup), deadRowsEstimate: number(r.n_dead_tup),
    lastAutovacuumAt: r.last_autovacuum, autovacuumCount: number(r.autovacuum_count)
  })));
  const tables = await optional(client, `WITH candidates AS (
      SELECT relid, schemaname, relname FROM pg_stat_user_tables
      ORDER BY n_live_tup DESC LIMIT 20
    ) SELECT schemaname, relname,
      pg_total_relation_size(relid) AS total_bytes,
      pg_indexes_size(relid) AS index_bytes
    FROM candidates ORDER BY total_bytes DESC LIMIT 10`, rows => rows.map(r => ({
    schema: r.schemaname, table: r.relname,
    totalBytes: number(r.total_bytes), indexBytes: number(r.index_bytes)
  })));
  const slots = await optional(client, `SELECT count(*)::int AS total,
    count(*) FILTER (WHERE active)::int AS active FROM pg_replication_slots`, rows => ({
    total: number(rows[0].total), active: number(rows[0].active)
  }));
  const freeze = await optional(client, `SELECT age(datfrozenxid)::bigint AS xid_age,
    current_setting('autovacuum_freeze_max_age')::bigint AS max_age
    FROM pg_database WHERE datname=current_database()`, rows => ({
    xidAge: number(rows[0].xid_age), maxAge: number(rows[0].max_age),
    usedPercent: Number((number(rows[0].xid_age) / number(rows[0].max_age) * 100).toFixed(1))
  }));
  const slotDetails = await optional(client, `SELECT slot_name, active, wal_status, safe_wal_size::text,
    ${inRecovery ? 'NULL::text' : 'CASE WHEN restart_lsn IS NOT NULL THEN pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::text END'} AS retained_bytes
    FROM pg_replication_slots ORDER BY active ASC, slot_name LIMIT 20`, rows => rows.map(row => ({
    name: row.slot_name, active: row.active, walStatus: row.wal_status,
    safeWalBytes: row.safe_wal_size == null ? null : number(row.safe_wal_size),
    retainedBytes: row.retained_bytes == null ? null : number(row.retained_bytes)
  })));
  const settings = await optional(client, `SELECT name, setting, unit, source FROM pg_settings
    WHERE name IN ('archive_mode','autovacuum','max_connections','max_slot_wal_keep_size','wal_level')
    ORDER BY name`, rows => rows.map(row => ({ name: row.name, value: row.setting,
    unit: row.unit, source: row.source })));
  return { activity, transactions, locks, lockChain, database, wal, archiver,
    checkpoints, vacuum, tables, slots, freeze, slotDetails, settings };
}
