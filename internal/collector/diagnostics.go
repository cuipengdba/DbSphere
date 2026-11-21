package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type diagnosticQuery struct {
	name string
	sql  string
}

// Every diagnostic is independent. A missing privilege or extension never hides
// PostgreSQL health; the unavailable status is retained in the raw sample.
func collectDiagnostics(ctx context.Context, conn *pgx.Conn, major int, inRecovery bool) map[string]any {
	checkpoint := `SELECT to_jsonb(q) FROM (
		SELECT checkpoints_timed AS timed, checkpoints_req AS requested FROM pg_stat_bgwriter) q`
	if major >= 17 {
		checkpoint = `SELECT to_jsonb(q) FROM (
			SELECT num_timed AS timed, num_requested AS requested FROM pg_stat_checkpointer) q`
	}
	retained := `CASE WHEN restart_lsn IS NOT NULL THEN
		pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::text END`
	if inRecovery {
		retained = `NULL::text`
	}
	queries := []diagnosticQuery{
		{"activity", `SELECT coalesce(jsonb_agg(to_jsonb(q)),'[]'::jsonb) FROM (
			SELECT coalesce(state,'unknown') AS state,coalesce(wait_event_type,'none') AS "waitType",
			count(*)::int AS sessions FROM pg_stat_activity WHERE datname=current_database()
			GROUP BY 1,2) q`},
		{"activeWaits", activeWaitsSQL},
		{"transactions", `SELECT to_jsonb(q) FROM (
			SELECT count(*) FILTER (WHERE xact_start < now()-interval '5 minutes')::int AS "longTransactions",
			count(*) FILTER (WHERE state='idle in transaction')::int AS "idleInTransaction"
			FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()) q`},
		{"locks", `SELECT to_jsonb(q) FROM (
			SELECT count(*)::int AS "blockedSessions" FROM pg_stat_activity a
			WHERE a.datname=current_database() AND a.pid<>pg_backend_pid()
			AND cardinality(pg_blocking_pids(a.pid))>0) q`},
		{"lockChain", `SELECT coalesce(jsonb_agg(to_jsonb(q)),'[]'::jsonb) FROM (
			SELECT a.pid AS "waitingPid",b.blocker_pid AS "blockerPid",
			a.wait_event_type AS "waitType",a.wait_event AS "waitEvent",
			a.query_id::text AS "waitingQueryId",
			CASE WHEN blocker.datid=a.datid THEN blocker.query_id::text END AS "blockerQueryId",
			a.xact_start AS "waitingSince",blocker.xact_start AS "blockerXactStart"
			FROM pg_stat_activity a CROSS JOIN LATERAL unnest(pg_blocking_pids(a.pid)) AS b(blocker_pid)
			LEFT JOIN pg_stat_activity blocker ON blocker.pid=b.blocker_pid
			WHERE a.datname=current_database() LIMIT 20) q`},
		{"database", `SELECT to_jsonb(q) FROM (
			SELECT deadlocks,blks_read AS "blocksRead",blks_hit AS "blocksHit",
			temp_bytes AS "tempBytes" FROM pg_stat_database WHERE datname=current_database()) q`},
		{"wal", `SELECT to_jsonb(q) FROM (
			SELECT wal_bytes::text AS bytes,wal_records AS records,wal_fpi AS "fullPageImages",
			wal_buffers_full AS "buffersFull" FROM pg_stat_wal) q`},
		{"archiver", `SELECT to_jsonb(q) FROM (
			SELECT archived_count AS "archivedCount",failed_count AS "failedCount",
			last_archived_time AS "lastArchivedAt",last_failed_time AS "lastFailedAt"
			FROM pg_stat_archiver) q`},
		{"checkpoints", checkpoint},
		{"vacuum", `SELECT coalesce(jsonb_agg(to_jsonb(q)),'[]'::jsonb) FROM (
			SELECT schemaname AS schema,relname AS "table",n_live_tup AS "liveRowsEstimate",
			n_dead_tup AS "deadRowsEstimate",last_autovacuum AS "lastAutovacuumAt",
			autovacuum_count AS "autovacuumCount" FROM pg_stat_user_tables
			ORDER BY n_dead_tup DESC LIMIT 10) q`},
		{"tables", `SELECT coalesce(jsonb_agg(to_jsonb(q)),'[]'::jsonb) FROM (
			WITH candidates AS (SELECT relid,schemaname,relname FROM pg_stat_user_tables
			ORDER BY n_live_tup DESC LIMIT 20)
			SELECT schemaname AS schema,relname AS "table",
			pg_total_relation_size(relid) AS "totalBytes",pg_indexes_size(relid) AS "indexBytes"
			FROM candidates ORDER BY "totalBytes" DESC LIMIT 10) q`},
		{"slots", `SELECT to_jsonb(q) FROM (
			SELECT count(*)::int AS total,count(*) FILTER (WHERE active)::int AS active
			FROM pg_replication_slots) q`},
		{"freeze", `SELECT to_jsonb(q) FROM (
			SELECT age(datfrozenxid)::bigint AS "xidAge",
			current_setting('autovacuum_freeze_max_age')::bigint AS "maxAge",
			round(age(datfrozenxid)::numeric /
			current_setting('autovacuum_freeze_max_age')::numeric*100,1) AS "usedPercent"
			FROM pg_database WHERE datname=current_database()) q`},
		{"slotDetails", fmt.Sprintf(`SELECT coalesce(jsonb_agg(to_jsonb(q)),'[]'::jsonb) FROM (
			SELECT slot_name AS name,active,wal_status AS "walStatus",
			safe_wal_size::text AS "safeWalBytes",%s AS "retainedBytes"
			FROM pg_replication_slots ORDER BY active ASC,slot_name LIMIT 20) q`, retained)},
		{"settings", `SELECT coalesce(jsonb_agg(to_jsonb(q)),'[]'::jsonb) FROM (
			SELECT name,setting AS value,unit,source FROM pg_settings
			WHERE name IN ('archive_mode','autovacuum','max_connections','max_slot_wal_keep_size','wal_level')
			ORDER BY name) q`},
	}
	result := make(map[string]any, len(queries))
	for _, query := range queries {
		var raw []byte
		err := conn.QueryRow(ctx, query.sql).Scan(&raw)
		if err != nil || raw == nil || string(raw) == "null" {
			reason := "query returned no data"
			if err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) {
					reason = "SQLSTATE " + pgErr.Code
				} else {
					reason = "query unavailable"
				}
			}
			result[query.name] = map[string]any{"status": "unavailable", "reason": reason}
			continue
		}
		var data any
		if err := json.Unmarshal(raw, &data); err != nil {
			result[query.name] = map[string]any{"status": "unavailable", "reason": "invalid diagnostic JSON"}
			continue
		}
		result[query.name] = map[string]any{"status": "ok", "data": data}
	}
	return result
}
