package collector

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrKingbaseVersion  = errors.New("KingbaseES V9 in PostgreSQL compatibility mode is required")
	ErrKingbaseReadOnly = errors.New("KingbaseES refused the read-only session")
	kingbaseVersion     = regexp.MustCompile(`^KingbaseES V009(?:R[0-9]{3}(?:C[0-9]{3}(?:B[0-9]{4})?)?)?(?:\b|$)`)
)

const kingbaseLockSource = "sys_catalog.sys_locks / sys_catalog.sys_blocking_pids"

type KingbaseWait struct {
	WaitingSession  string     `json:"waitingSession"`
	BlockingSession string     `json:"blockingSession"`
	Event           string     `json:"event"`
	WaitingSince    *time.Time `json:"waitingQueryStarted,omitempty"`
	BlockingSince   *time.Time `json:"blockingTransactionStarted,omitempty"`
}

type KingbaseMetrics struct {
	Version          string          `json:"version"`
	Mode             string          `json:"mode"`
	StartedAt        time.Time       `json:"startedAt"`
	Sessions         *int            `json:"sessions"`
	ActiveSessions   *int            `json:"activeSessions"`
	Transactions     *int            `json:"transactions"`
	DatabaseBytes    *int64          `json:"databaseBytes"`
	Waits            []KingbaseWait  `json:"waits"`
	WaitsTruncated   bool            `json:"waitsTruncated"`
	WaitCategories   json.RawMessage `json:"waitCategories"`
	LongTransactions json.RawMessage `json:"longTransactions"`
	Replication      json.RawMessage `json:"replication"`
	TopSQL           json.RawMessage `json:"topSql"`
	SessionWaits     json.RawMessage `json:"sessionWaits"`
	HA               *NativeHA       `json:"ha,omitempty"`
}

type KingbaseAdapter struct{}

var _ collectoradapter.Adapter = KingbaseAdapter{}

func (KingbaseAdapter) Collect(ctx context.Context, target Target, password string) (collectoradapter.Snapshot, error) {
	config, err := readOnlyPostgresConfig(target, password)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	config.RuntimeParams["search_path"] = "pg_catalog"
	ctx, cancel := context.WithTimeout(ctx, 14*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	defer conn.Close(ctx)
	var details KingbaseMetrics
	var readOnly, stats bool
	err = conn.QueryRow(ctx, `SELECT version(),coalesce(current_setting('database_mode',true),''),
		current_setting('transaction_read_only')='on'`).Scan(&details.Version, &details.Mode, &readOnly)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	if !kingbaseVersion.MatchString(details.Version) || details.Mode != "pg" {
		return collectoradapter.Snapshot{}, ErrKingbaseVersion
	}
	if !readOnly {
		return collectoradapter.Snapshot{}, ErrKingbaseReadOnly
	}
	var standby bool
	err = conn.QueryRow(ctx, `SELECT sys_catalog.sys_is_in_recovery(),sys_catalog.sys_postmaster_start_time(),
		EXISTS(SELECT 1 FROM pg_roles
			WHERE rolname IN ('pg_read_all_stats','sys_read_all_stats') AND pg_has_role(oid,'USAGE'))`).
		Scan(&standby, &details.StartedAt, &stats)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	role := "primary"
	if standby {
		role = "standby"
	}
	report := postgresAdapterReport(ctx, target, conn, 90000, role, readOnly, nil)
	report.Engine, report.Product, report.Profile = "kingbase", details.Version, "kingbase-v9-pg"
	recordSignal := func(key, source string, err error) {
		signal := collectoradapter.Signal{Key: key, Source: source, Status: "ok"}
		if err != nil {
			signal.Status, signal.Reason = "unavailable", "采集失败"
			var remote *pgconn.PgError
			if errors.As(err, &remote) {
				signal.Reason += " · SQLSTATE " + remote.Code
				if remote.Code == "42501" {
					signal.Status = "permission_denied"
				}
			}
		}
		report.Signals = append(report.Signals, signal)
	}
	limited := func(key, source string) {
		report.Signals = append(report.Signals, collectoradapter.Signal{Key: key, Source: source,
			Status: "permission_denied", Reason: "需要统计视图读取权限"})
	}
	err = conn.QueryRow(ctx, `SELECT sys_catalog.sys_database_size(current_database())::bigint`).Scan(&details.DatabaseBytes)
	recordSignal("databaseSize", "sys_catalog.sys_database_size", err)
	var endpoint string
	err = conn.QueryRow(ctx, `SELECT coalesce(host(inet_server_addr())||':'||inet_server_port()::text,'')`).Scan(&endpoint)
	if err == nil && endpoint != "" {
		details.HA = &NativeHA{Node: endpoint, Role: role, Unit: "bytes", Source: "sys_catalog.sys_is_in_recovery"}
	}
	const activitySource = "sys_catalog.sys_stat_activity"
	const sqlSource = "sys_stat_statements"
	if !stats {
		limited("sessions", activitySource)
		limited("sessionWaits", activitySource)
		limited("lockWaits", kingbaseLockSource)
		limited("replication", "sys_catalog.sys_stat_replication / sys_stat_wal_receiver")
		limited("replicationDetail", "sys_catalog.sys_stat_replication / sys_stat_wal_receiver")
		limited("archive", "sys_catalog.sys_stat_archiver")
		limited("topSql", sqlSource)
	} else {
		err = conn.QueryRow(ctx, kingbaseSessionsSQL).Scan(&details.Sessions, &details.ActiveSessions, &details.Transactions,
			&details.WaitCategories, &details.LongTransactions)
		recordSignal("sessions", activitySource, err)
		err = conn.QueryRow(ctx, kingbaseSessionWaitsSQL).Scan(&details.SessionWaits)
		recordSignal("sessionWaits", activitySource, err)
		var locks []byte
		err = conn.QueryRow(ctx, kingbaseLocksSQL).Scan(&locks)
		if err == nil {
			err = json.Unmarshal(locks, &details.Waits)
		}
		if len(details.Waits) > 32 {
			details.WaitsTruncated = true
			details.Waits = details.Waits[:32]
		}
		recordSignal("lockWaits", kingbaseLockSource, err)
		replicationSource, replicationSQL := "sys_catalog.sys_stat_replication", kingbaseSenderSQL
		if standby {
			replicationSource, replicationSQL = "sys_catalog.sys_stat_wal_receiver", kingbaseReceiverSQL
		}
		err = conn.QueryRow(ctx, replicationSQL).Scan(&details.Replication)
		recordSignal("replication", replicationSource, err)
		if details.HA != nil {
			kingbaseCollectHA(ctx, conn, details.HA, standby, replicationSource, recordSignal)
		}
		var schema, tracking string
		err = conn.QueryRow(ctx, `SELECT n.nspname,coalesce(current_setting('sys_stat_statements.track',true),'none')
			FROM pg_extension e JOIN pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='sys_stat_statements'`).Scan(&schema, &tracking)
		switch {
		case errors.Is(err, pgx.ErrNoRows) || (err == nil && tracking == "none"):
			reason := "SQL 统计已关闭"
			if errors.Is(err, pgx.ErrNoRows) {
				reason = "当前数据库未安装 SQL 统计扩展"
			}
			report.Signals = append(report.Signals, collectoradapter.Signal{Key: "topSql", Source: sqlSource,
				Status: "not_configured", Reason: reason})
		default:
			if err == nil {
				view := pgx.Identifier{schema, "sys_stat_statements"}.Sanitize()
				err = conn.QueryRow(ctx, `SELECT coalesce(jsonb_agg(to_jsonb(q) ORDER BY q."totalMs" DESC),'[]'::jsonb) FROM (
					SELECT dbid::text||':'||userid::text||':'||queryid::text AS fingerprint,calls,
						total_exec_time AS "totalMs",mean_exec_time AS "meanMs",max_exec_time AS "maxMs"
					FROM `+view+` WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())
					AND userid<>(SELECT oid FROM pg_roles WHERE rolname=current_user)
					AND queryid IS NOT NULL AND calls>0 ORDER BY total_exec_time DESC,queryid,userid LIMIT 20) q`).Scan(&details.TopSQL)
			}
			recordSignal("topSql", sqlSource, err)
		}
	}
	return collectoradapter.Snapshot{Report: report, Metrics: map[string]any{
		"kingbase": details, "version_num": 90000, "role": role,
	}}, nil
}

const kingbaseSessionsSQL = `WITH activity AS (
	SELECT pid,state,xact_start,wait_event_type FROM sys_catalog.sys_stat_activity
	WHERE datname=current_database() AND backend_type='client backend' AND pid<>sys_catalog.sys_backend_pid()
), waits AS (
	SELECT wait_event_type AS category,count(*)::int AS count FROM activity
	WHERE state<>'idle' AND wait_event_type IS NOT NULL AND wait_event_type<>'Client' GROUP BY wait_event_type
), transactions AS (
	SELECT pid::text AS session,state,xact_start AS "startedAt",
		(extract(epoch FROM (now()-xact_start))*1000)::bigint AS "ageMs"
	FROM activity WHERE xact_start < now()-interval '10 seconds' ORDER BY xact_start,pid LIMIT 20
)
SELECT count(*)::int,count(*) FILTER(WHERE state='active')::int,count(xact_start)::int,
	(SELECT coalesce(jsonb_agg(to_jsonb(waits) ORDER BY count DESC,category),'[]'::jsonb) FROM waits),
	(SELECT coalesce(jsonb_agg(to_jsonb(transactions) ORDER BY "ageMs" DESC,session),'[]'::jsonb) FROM transactions)
FROM activity`

const kingbaseLocksSQL = `WITH waiting AS (
	SELECT DISTINCT a.pid,a.wait_event,a.query_start
	FROM sys_catalog.sys_stat_activity a JOIN sys_catalog.sys_locks l ON l.pid=a.pid AND NOT l.granted
	WHERE a.datname=current_database() AND a.pid<>sys_catalog.sys_backend_pid()
	ORDER BY a.query_start NULLS LAST,a.pid LIMIT 33
), pairs AS (
	SELECT DISTINCT w.pid::text AS "waitingSession", holder.pid::text AS "blockingSession",
		coalesce(w.wait_event,'Lock') AS event,w.query_start AS "waitingQueryStarted",
		b.xact_start AS "blockingTransactionStarted"
	FROM waiting w CROSS JOIN LATERAL unnest(sys_catalog.sys_blocking_pids(w.pid)) holder(pid)
	LEFT JOIN sys_catalog.sys_stat_activity b ON b.pid=holder.pid
	ORDER BY "waitingQueryStarted" NULLS LAST,"waitingSession","blockingSession" LIMIT 33
)
SELECT coalesce(jsonb_agg(to_jsonb(pairs) ORDER BY "waitingQueryStarted" NULLS LAST,"waitingSession","blockingSession"),'[]'::jsonb) FROM pairs`

const kingbaseSenderSQL = `SELECT jsonb_build_object('connected',count(*)::int,
	'streaming',count(*) FILTER(WHERE state='streaming')::int,
	'maxReplayLagBytes',max(sys_catalog.sys_wal_lsn_diff(sys_catalog.sys_current_wal_lsn(),replay_lsn))::bigint)
	FROM sys_catalog.sys_stat_replication`

const kingbaseSessionWaitsSQL = `SELECT coalesce(jsonb_agg(to_jsonb(w) ORDER BY w.session),'[]'::jsonb) FROM (
	SELECT a.pid::text AS session,left(a.wait_event_type,96) AS category,left(a.wait_event,96) AS event,
		coalesce(to_jsonb(a)->>'query_id','') AS "queryId",a.backend_start AS "backendStartedAt"
	FROM sys_catalog.sys_stat_activity a WHERE a.datname=current_database() AND a.pid<>sys_catalog.sys_backend_pid()
		AND a.backend_type='client backend' AND a.state='active' AND a.wait_event_type IS NOT NULL AND a.wait_event_type<>'Client'
	ORDER BY a.query_start NULLS LAST,a.pid LIMIT 32
) w`

const kingbaseReceiverSQL = `SELECT coalesce((SELECT jsonb_build_object('receiver',status)
	FROM sys_catalog.sys_stat_wal_receiver LIMIT 1),jsonb_build_object('receiver','absent'))`

func kingbaseCollectHA(ctx context.Context, conn *pgx.Conn, ha *NativeHA, standby bool, source string,
	recordSignal func(string, string, error)) {
	var peerJSON []byte
	query := kingbaseHASenderSQL
	if standby {
		query = kingbaseHAReceiverSQL
	}
	err := conn.QueryRow(ctx, query).Scan(&ha.Current, &ha.Received, &ha.Replayed, &ha.ReplayGap, &peerJSON)
	if err == nil {
		err = json.Unmarshal(peerJSON, &ha.Peers)
		if err == nil {
			ha.PeersObserved = true
			ha.Source = source
		}
	}
	if len(ha.Peers) > 32 {
		ha.Peers = ha.Peers[:32]
		ha.PeersTruncated = true
	}
	recordSignal("replicationDetail", source, err)
	var archive NativeArchive
	err = conn.QueryRow(ctx, `SELECT archived_count,failed_count,coalesce(last_archived_time::text,''),
		coalesce(last_failed_time::text,''),coalesce(stats_reset::text,''),current_setting('archive_mode'),
		CASE WHEN current_setting('archive_mode')='off' THEN NULL ELSE
		last_failed_time IS NOT NULL AND (last_archived_time IS NULL OR last_failed_time>last_archived_time) END
		FROM sys_catalog.sys_stat_archiver`).Scan(&archive.Archived, &archive.Failed, &archive.LastOK, &archive.LastFail, &archive.ResetAt, &archive.State, &archive.Error)
	if err == nil {
		ha.Archive = &archive
	}
	recordSignal("archive", "sys_catalog.sys_stat_archiver", err)
}

const kingbaseHASenderSQL = `SELECT sys_catalog.sys_current_wal_lsn()::text,''::text,''::text,NULL::bigint,
	coalesce((SELECT jsonb_agg(to_jsonb(p) ORDER BY name) FROM (
		SELECT left(application_name,128) AS name,'downstream' AS kind,coalesce(state,'') AS state,
			coalesce(sync_state,'') AS sync,sent_lsn::text AS sent,write_lsn::text AS received,
			flush_lsn::text AS flushed,replay_lsn::text AS replayed,
			CASE WHEN sent_lsn>=write_lsn THEN sys_catalog.sys_wal_lsn_diff(sent_lsn,write_lsn)::bigint END AS "transferGap",
			CASE WHEN flush_lsn>=replay_lsn THEN sys_catalog.sys_wal_lsn_diff(flush_lsn,replay_lsn)::bigint END AS "replayGap"
		FROM sys_catalog.sys_stat_replication ORDER BY application_name,pid LIMIT 33
	) p),'[]'::jsonb)`

const kingbaseHAReceiverSQL = `WITH positions AS (SELECT sys_catalog.sys_last_wal_receive_lsn() AS received,
	sys_catalog.sys_last_wal_replay_lsn() AS replayed)
	SELECT ''::text,coalesce(received::text,''),coalesce(replayed::text,''),
		CASE WHEN received>=replayed THEN sys_catalog.sys_wal_lsn_diff(received,replayed)::bigint END,
		coalesce((SELECT jsonb_agg(to_jsonb(p)) FROM (
			SELECT left(coalesce(to_jsonb(r)->>'sender_host','')||':'||coalesce(to_jsonb(r)->>'sender_port',''),128) AS name,
				'upstream' AS kind,status AS state,received::text AS flushed,replayed::text AS replayed,
				coalesce(to_jsonb(r)->>'written_lsn',to_jsonb(r)->>'received_lsn') AS received,to_jsonb(r)->>'latest_end_lsn' AS sent
			FROM sys_catalog.sys_stat_wal_receiver r LIMIT 1
		) p),'[]'::jsonb) FROM positions`
