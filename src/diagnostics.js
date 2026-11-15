const number = value => value == null ? null : Number(value);

async function optional(client, sql, map) {
  try { return { status: 'ok', data: map((await client.query(sql)).rows) }; }
  catch (error) { return { status: 'unavailable', reason: String(error.code || error.message).slice(0, 120) }; }
}

export async function collectDiagnostics(client, major) {
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
  return { activity, transactions, locks, lockChain, database, wal, archiver, checkpoints, vacuum, tables, slots };
}
