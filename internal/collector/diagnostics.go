package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type diagnosticQuery struct {
	name string
	sql  string
}

var legacyActivityColumns = strings.NewReplacer(
	"a.leader_pid", "NULL::int",
	"a.query_id::text", "NULL::text", "blocker.query_id::text", "NULL::text", "query_id::text", "NULL::text",
)

// Every diagnostic is independent. A missing privilege or extension never hides
// PostgreSQL health; the unavailable status is retained in the raw sample.
func collectDiagnostics(ctx context.Context, conn *pgx.Conn, major int, inRecovery bool) map[string]any {
	checkpoint := `SELECT to_jsonb(q) FROM (
		SELECT checkpoints_timed AS timed, checkpoints_req AS requested,
		checkpoint_write_time AS "writeMs",checkpoint_sync_time AS "syncMs",buffers_checkpoint AS "writtenBuffers",
		stats_reset AS "statsReset" FROM pg_stat_bgwriter) q`
	if major >= 17 {
		checkpoint = `SELECT to_jsonb(q) FROM (
			SELECT num_timed AS timed, num_requested AS requested,write_time AS "writeMs",sync_time AS "syncMs",
			buffers_written AS "writtenBuffers",stats_reset AS "statsReset" FROM pg_stat_checkpointer) q`
	}
	retained := `CASE WHEN restart_lsn IS NOT NULL THEN
		pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::text END`
	backlog := `CASE WHEN confirmed_flush_lsn IS NOT NULL THEN
		greatest(0,pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn))::text END`
	replication := diagnosticQuery{"replicationLinks", replicationLinksSQL}
	walPosition := diagnosticQuery{"walPosition", `SELECT jsonb_build_object(
		'position',pg_current_wal_lsn()::text,'timeline',left(pg_walfile_name(pg_current_wal_lsn()),8),
		'timelineSource','pg_walfile_name','startedAt',pg_postmaster_start_time())`}
	if inRecovery {
		retained = `NULL::text`
		backlog = `NULL::text`
		replication = diagnosticQuery{"walReceiver", `SELECT to_jsonb(q) FROM (
			SELECT status, sender_host AS "senderHost", sender_port AS "senderPort", slot_name AS slot,
			written_lsn::text AS "writtenLsn",flushed_lsn::text AS "flushedLsn",latest_end_lsn::text AS "senderLsn",
			last_msg_receipt_time AS "lastMessageAt" FROM pg_stat_wal_receiver) q`}
		if major < 13 {
			replication.sql = strings.NewReplacer("written_lsn::text", "NULL::text", "flushed_lsn::text", "received_lsn::text").Replace(replication.sql)
		}
		walPosition.sql = `SELECT jsonb_build_object('position',pg_last_wal_replay_lsn()::text,
			'received',pg_last_wal_receive_lsn()::text,'replayPaused',pg_is_wal_replay_paused(),
			'localReplayGapBytes',pg_wal_lsn_diff(pg_last_wal_receive_lsn(),pg_last_wal_replay_lsn()),
			'timeline',(SELECT upper(lpad(to_hex(received_tli),8,'0')) FROM pg_stat_wal_receiver),
			'timelineSource','pg_stat_wal_receiver.received_tli','startedAt',pg_postmaster_start_time())`
	}
	queries := []diagnosticQuery{
		{"activity", `SELECT coalesce(jsonb_agg(to_jsonb(q)),'[]'::jsonb) FROM (
			SELECT coalesce(state,'unknown') AS state,coalesce(wait_event_type,'none') AS "waitType",
			count(*)::int AS sessions FROM pg_stat_activity WHERE datname=current_database()
			GROUP BY 1,2) q`},
		{"activeWaits", activeWaitsSQL},
		{"instanceSessions", `SELECT to_jsonb(q) FROM (
			SELECT count(*)::int AS total,
			round(count(*)::numeric/current_setting('max_connections')::int*100,1) AS "usedPercent",
			CASE WHEN count(*) FILTER (WHERE state IS NULL)=0 THEN count(*) FILTER (WHERE state='active') END AS active,
			CASE WHEN count(*) FILTER (WHERE state IS NULL)=0 THEN count(*) FILTER (WHERE state='active' AND wait_event_type IS NOT NULL) END AS waiting
			FROM pg_stat_activity WHERE backend_type='client backend' AND pid<>pg_backend_pid()) q`},
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
			temp_bytes AS "tempBytes",temp_files AS "tempFiles",stats_reset AS "statsReset",
			CASE WHEN current_setting('track_io_timing')='on' THEN blk_read_time END AS "readTimeMs",
			CASE WHEN current_setting('track_io_timing')='on' THEN blk_write_time END AS "writeTimeMs"
			FROM pg_stat_database WHERE datname=current_database()) q`},
		{"wal", `SELECT to_jsonb(q) FROM (
			SELECT wal_bytes::text AS bytes,wal_records AS records,wal_fpi AS "fullPageImages",
			wal_buffers_full AS "buffersFull",stats_reset AS "statsReset" FROM pg_stat_wal) q`},
		{"archiver", `SELECT to_jsonb(q) FROM (
			SELECT archived_count AS "archivedCount",failed_count AS "failedCount",
			last_archived_time AS "lastArchivedAt",last_failed_time AS "lastFailedAt",stats_reset AS "statsReset"
			FROM pg_stat_archiver) q`},
		replication, walPosition,
		{"checkpoints", checkpoint},
		{"vacuum", `SELECT coalesce(jsonb_agg(to_jsonb(q)),'[]'::jsonb) FROM (
			SELECT s.relid::text AS "tableOid",(SELECT oid::text FROM pg_database WHERE datname=current_database()) AS "databaseOid",
			s.schemaname AS schema,s.relname AS "table",n_live_tup AS "liveRowsEstimate",
			n_dead_tup AS "deadRowsEstimate",last_autovacuum AS "lastAutovacuumAt",
			autovacuum_count AS "autovacuumCount" FROM pg_stat_user_tables s JOIN pg_class c ON c.oid=s.relid
			WHERE pg_has_role(current_user,'pg_read_all_stats','member') OR
				(has_schema_privilege(c.relnamespace,'USAGE') AND has_table_privilege(c.oid,'SELECT'))
			ORDER BY n_dead_tup DESC,s.relid LIMIT 20) q`},
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
			SELECT s.slot_name AS name,s.active,to_jsonb(s)->>'wal_status' AS "walStatus",s.slot_type AS type,s.database,s.plugin,
			s.restart_lsn::text AS "restartLsn",s.confirmed_flush_lsn::text AS "confirmedFlushLsn",
			to_jsonb(s)->>'safe_wal_size' AS "safeWalBytes",%s AS "retainedBytes",%s AS "unconfirmedBytes",
			extract(epoch FROM r.replay_lag) AS "replayLagSeconds",r.reply_time AS "lastReplyAt",
			to_jsonb(s)->>'invalidation_reason' AS "invalidationReason",to_jsonb(s)->>'inactive_since' AS "inactiveSince"
			FROM pg_catalog.pg_replication_slots s LEFT JOIN pg_catalog.pg_stat_replication r ON r.pid=s.active_pid
			ORDER BY s.active ASC,s.restart_lsn ASC NULLS LAST,s.slot_name LIMIT 40) q`, retained, backlog)},
		{"logicalPublications", logicalPublicationsSQL},
		{"logicalSubscriptions", logicalSubscriptionsSQL},
		{"logicalApplyProgress", logicalApplyProgressSQL},
		{"settings", `SELECT coalesce(jsonb_agg(to_jsonb(q)),'[]'::jsonb) FROM (
			SELECT name,setting AS value,unit,source FROM pg_settings
			WHERE name IN ('archive_mode','autovacuum','log_min_duration_statement','max_connections','max_slot_wal_keep_size','wal_level')
			ORDER BY name) q`},
		{"databaseInventory", databaseInventorySQL},
		{"tableInventory", tableInventorySQL},
		{"indexInventory", indexInventorySQL},
	}
	if !inRecovery {
		queries = append(queries, diagnosticQuery{"logicalSlotPressure", logicalSlotPressureSQL})
	}
	if major >= 15 {
		queries = append(queries, diagnosticQuery{"logicalSubscriptionErrors", logicalSubscriptionErrorsSQL})
	}
	if major >= 16 {
		queries = append(queries, diagnosticQuery{"instanceIO", instanceIOSQL})
	}
	result := make(map[string]any, len(queries))
	for _, query := range queries {
		if major < 14 {
			switch query.name {
			case "activeWaits", "lockChain":
				query.sql = legacyActivityColumns.Replace(query.sql)
			case "tableInventory":
				query.sql = strings.Replace(query.sql, "s.n_ins_since_vacuum", "NULL::bigint", 1)
			case "wal":
				result[query.name] = map[string]any{"status": "not_supported", "reason": "WAL counters require pg_stat_wal"}
				continue
			}
		}
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

const replicationLinksSQL = `SELECT jsonb_build_object('links',coalesce(jsonb_agg(to_jsonb(q) ORDER BY q."replayGapBytes" DESC NULLS LAST,q.pid),'[]'::jsonb),
	'total',(SELECT count(*) FROM pg_stat_replication)) FROM (
	SELECT pid,application_name AS application,host(client_addr) AS "clientAddress",backend_start AS "connectedAt",
		state,sync_state AS "syncState",sent_lsn::text AS "sentLsn",write_lsn::text AS "writeLsn",
		flush_lsn::text AS "flushLsn",replay_lsn::text AS "replayLsn",reply_time AS "lastReplyAt",
		pg_wal_lsn_diff(pg_current_wal_lsn(),replay_lsn) AS "replayGapBytes",
		extract(epoch FROM write_lag) AS "writeLagSeconds",extract(epoch FROM flush_lag) AS "flushLagSeconds",
		extract(epoch FROM replay_lag) AS "replayLagSeconds"
	FROM pg_stat_replication ORDER BY "replayGapBytes" DESC NULLS LAST,pid LIMIT 50) q`
