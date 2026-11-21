package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidRange = errors.New("invalid monitoring time range")

func (s *Postgres) ReadAssetSection(ctx context.Context, id, section string, params url.Values) (any, error) {
	switch section {
	case "samples":
		values, err := s.queryJSON(ctx, `SELECT jsonb_build_object(
			'id',id,'assetId',asset_id,'collectedAt',collected_at,'status',status,
			'metrics',metrics_json,'error',error)
			FROM samples WHERE asset_id=$1 ORDER BY collected_at DESC LIMIT 100`, id)
		return map[string]any{"samples": values}, err
	case "sql":
		if params.Get("from") != "" || params.Get("to") != "" {
			from, to, err := timeRange(params, 0, 7*24*time.Hour)
			if err != nil || len(params.Get("queryId")) > 32 {
				return nil, ErrInvalidRange
			}
			return s.sqlSummary(ctx, id, &from, &to, params.Get("queryId"))
		}
		return s.SQLSummary(ctx, id)
	case "wait-history":
		return s.waitHistory(ctx, id, params)
	case "sql-window":
		return s.sqlWindow(ctx, id, params)
	case "monitoring-context":
		return s.monitoringContext(ctx, id, params)
	case "replication":
		return s.replicationHistory(ctx, id, params)
	case "catalog":
		return s.databaseCatalog(ctx, id, params)
	case "slow-logs":
		from, to, err := timeRange(params, 24*time.Hour, 31*24*time.Hour)
		if err != nil {
			return nil, err
		}
		values, err := s.queryJSON(ctx, `SELECT jsonb_build_object(
			'id',l.id,'assetId',l.asset_id,'observedAt',l.observed_at,
			'durationMs',l.duration_ms,'queryId',l.query_id,'sourceFormat',l.source_format,
			'eventIds',coalesce((SELECT jsonb_agg(e.id) FROM events e
				WHERE e.asset_id=l.asset_id AND e.started_at<=l.observed_at+interval '5 minutes'
				AND (e.ended_at IS NULL OR e.ended_at>=l.observed_at-interval '5 minutes')),'[]'::jsonb))
			FROM slow_log_entries l WHERE l.asset_id=$1 AND l.observed_at>=$2 AND l.observed_at<$3
			AND ($4::text='' OR l.query_id=$4)
			ORDER BY l.observed_at DESC LIMIT 100`, id, from, to, params.Get("queryId"))
		return map[string]any{"entries": values}, err
	case "sql-plans":
		values, err := s.queryJSON(ctx, `SELECT jsonb_build_object(
			'id',p.id,'assetId',p.asset_id,'queryId',p.query_id,
			'observedAt',p.observed_at,'source',p.source,'shapeHash',p.shape_hash,
			'summary',p.summary_json,
			'eventIds',coalesce((SELECT jsonb_agg(e.id) FROM events e
				WHERE e.asset_id=p.asset_id AND e.started_at<=p.observed_at+interval '5 minutes'
				AND (e.ended_at IS NULL OR e.ended_at>=p.observed_at-interval '5 minutes')),'[]'::jsonb))
			FROM sql_plan_snapshots p WHERE p.asset_id=$1
			AND ($2::text='' OR p.query_id=$2) ORDER BY p.observed_at DESC LIMIT 50`, id, params.Get("queryId"))
		if err != nil {
			return nil, err
		}
		plans := make([]map[string]any, len(values))
		for i, raw := range values {
			if err := json.Unmarshal(raw, &plans[i]); err != nil {
				return nil, err
			}
		}
		prior := map[string]map[string]any{}
		for i := len(plans) - 1; i >= 0; i-- {
			current := plans[i]
			queryID, _ := current["queryId"].(string)
			if old := prior[queryID]; old != nil {
				comparison := map[string]any{"priorId": old["id"],
					"shapeChanged": old["shapeHash"] != current["shapeHash"], "executionRatio": nil}
				currentSummary, _ := current["summary"].(map[string]any)
				priorSummary, _ := old["summary"].(map[string]any)
				currentMs, _ := currentSummary["executionMs"].(float64)
				priorMs, _ := priorSummary["executionMs"].(float64)
				if currentMs > 0 && priorMs > 0 {
					comparison["executionRatio"] = currentMs / priorMs
				}
				current["comparison"] = comparison
			} else {
				current["comparison"] = nil
			}
			prior[queryID] = current
		}
		return map[string]any{"plans": plans}, nil
	case "series":
		if params.Get("from") == "" || params.Get("to") == "" {
			return nil, ErrInvalidRange
		}
		from, to, err := timeRange(params, 0, 90*24*time.Hour)
		if err != nil {
			return nil, err
		}
		bucket := 60
		if params.Get("bucketHours") != "" {
			bucket, err = strconv.Atoi(params.Get("bucketHours"))
			if err != nil || bucket < 1 || bucket > 24 {
				return nil, ErrInvalidRange
			}
			bucket *= 60
		}
		if raw := params.Get("bucketMinutes"); raw != "" {
			bucket, err = strconv.Atoi(raw)
			if err != nil || (bucket != 1 && bucket != 5 && bucket != 15 && bucket != 60) ||
				params.Get("bucketHours") != "" || to.Sub(from) > time.Duration(bucket*400)*time.Minute {
				return nil, ErrInvalidRange
			}
		}
		values, err := s.readMonitoringBuckets(ctx, []string{id}, from, to, bucket)
		return map[string]any{"series": values}, err
	case "capacity":
		forecast, err := s.CapacityForecast(ctx, id)
		return map[string]any{"forecast": forecast}, err
	default:
		return nil, fmt.Errorf("unsupported asset section")
	}
}

func (s *Postgres) readMonitoringBuckets(ctx context.Context, ids []string, from, to time.Time, bucket int) ([]json.RawMessage, error) {
	return s.queryJSON(ctx, `WITH observed AS (
			SELECT id,asset_id,collected_at,status,
				metrics_json#>>'{collector,pgStatus}' AS pg_status,
				CASE WHEN status='ok' THEN metrics_json->>'role' END AS pg_role,
				metrics_json#>>'{diagnostics,wal,data,statsReset}' AS wal_reset,
				metrics_json#>>'{diagnostics,walPosition,data,startedAt}' AS server_started,
				metrics_json#>>'{diagnostics,walPosition,data,timeline}' AS wal_timeline,
				CASE WHEN jsonb_typeof(metrics_json#>'{diagnostics,freeze,data,usedPercent}')='number'
					THEN (metrics_json#>>'{diagnostics,freeze,data,usedPercent}')::numeric END AS xid_used,
				CASE WHEN metrics_json#>>'{linux,status}'='ok' AND jsonb_typeof(metrics_json#>'{linux,cpu,usedPercent}')='number'
					THEN (metrics_json#>>'{linux,cpu,usedPercent}')::numeric END AS linux_cpu,
				CASE WHEN metrics_json#>>'{linux,status}'='ok' AND jsonb_typeof(metrics_json#>'{linux,memory,usedPercent}')='number'
					THEN (metrics_json#>>'{linux,memory,usedPercent}')::numeric END AS linux_memory,
				CASE WHEN status='ok' AND jsonb_typeof(metrics_json->'connections')='number'
					THEN (metrics_json->>'connections')::numeric END AS connections,
				CASE WHEN status='ok' AND jsonb_typeof(metrics_json->'max_connections')='number'
					THEN (metrics_json->>'max_connections')::numeric END AS max_connections,
				CASE WHEN status='ok' AND jsonb_typeof(metrics_json->'database_bytes')='number'
					THEN (metrics_json->>'database_bytes')::numeric END AS database_bytes,
				CASE WHEN status='ok' AND jsonb_typeof(metrics_json->'replication_lag_bytes')='number'
					THEN (metrics_json->>'replication_lag_bytes')::numeric END AS replication_lag_bytes,
				CASE WHEN status='ok' AND jsonb_typeof(metrics_json#>'{diagnostics,locks,data,blockedSessions}')='number'
					THEN (metrics_json#>>'{diagnostics,locks,data,blockedSessions}')::numeric END AS blocked_sessions,
				CASE WHEN status='ok' AND jsonb_typeof(metrics_json#>'{diagnostics,transactions,data,longTransactions}')='number'
					THEN (metrics_json#>>'{diagnostics,transactions,data,longTransactions}')::numeric END AS long_transactions,
				CASE WHEN metrics_json#>>'{diagnostics,transactions,status}'='ok'
					AND jsonb_typeof(metrics_json#>'{diagnostics,transactions,data,idleInTransaction}')='number'
					THEN (metrics_json#>>'{diagnostics,transactions,data,idleInTransaction}')::numeric END AS idle_in_transaction,
				CASE WHEN metrics_json#>>'{diagnostics,database,status}'='ok'
					AND jsonb_typeof(metrics_json#>'{diagnostics,database,data,deadlocks}')='number'
					THEN (metrics_json#>>'{diagnostics,database,data,deadlocks}')::numeric END AS deadlocks,
				CASE WHEN status='ok' AND jsonb_typeof(metrics_json->'xact_commit')='number'
					THEN (metrics_json->>'xact_commit')::numeric END AS commits,
				CASE WHEN status='ok' AND jsonb_typeof(metrics_json->'xact_rollback')='number'
					THEN (metrics_json->>'xact_rollback')::numeric END AS rollbacks,
				CASE WHEN status='ok' AND (metrics_json#>>'{diagnostics,wal,data,bytes}') ~ '^[0-9]+([.][0-9]+)?$'
					THEN (metrics_json#>>'{diagnostics,wal,data,bytes}')::numeric END AS wal_bytes
			FROM samples WHERE asset_id=ANY($1::uuid[]) AND collected_at>=$2 AND collected_at<$3
		), compared AS (
			SELECT *,lag(collected_at) OVER observed_order AS prior_at,
				lag(commits) OVER observed_order AS prior_commits,
				lag(rollbacks) OVER observed_order AS prior_rollbacks,
				lag(deadlocks) OVER observed_order AS prior_deadlocks,
				lag(wal_bytes) OVER observed_order AS prior_wal_bytes,
				lag(wal_reset) OVER observed_order AS prior_wal_reset,
				lag(server_started) OVER observed_order AS prior_started,
				lag(wal_timeline) OVER observed_order AS prior_timeline,
				lag(pg_role) OVER observed_order AS prior_role
			FROM observed WINDOW observed_order AS (PARTITION BY asset_id ORDER BY collected_at,id)
		), rated AS (
			SELECT *,CASE WHEN commits>=prior_commits AND rollbacks>=prior_rollbacks
				AND extract(epoch FROM collected_at-prior_at) BETWEEN 1 AND 600
				THEN ((commits-prior_commits)+(rollbacks-prior_rollbacks)) /
					extract(epoch FROM collected_at-prior_at) END AS transactions_per_second,
				CASE WHEN deadlocks>=prior_deadlocks AND extract(epoch FROM collected_at-prior_at) BETWEEN 1 AND 600
					THEN deadlocks-prior_deadlocks END AS deadlock_delta,
				CASE WHEN wal_bytes>=prior_wal_bytes
				AND wal_reset IS NOT DISTINCT FROM prior_wal_reset AND server_started IS NOT DISTINCT FROM prior_started
				AND wal_timeline IS NOT DISTINCT FROM prior_timeline AND pg_role IS NOT DISTINCT FROM prior_role
				AND extract(epoch FROM collected_at-prior_at) BETWEEN 1 AND 600
				THEN (wal_bytes-prior_wal_bytes) /
					extract(epoch FROM collected_at-prior_at) END AS wal_bytes_per_second
			FROM compared
		)
		SELECT jsonb_build_object(
			'assetId',asset_id,'bucket',date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz),
			'samples',count(*)::int,'errors',count(*) FILTER (WHERE status='error')::int,
			'pgAvailable',count(*) FILTER (WHERE status='ok' OR pg_status='ok')::int,
			'pgUnavailable',count(*) FILTER (WHERE pg_status='availability')::int,
			'observedRole',CASE WHEN count(DISTINCT pg_role)=1 AND count(pg_role)=count(*) THEN min(pg_role) END,
			'xidUsedPercent',round(max(xid_used),1),
			'averageConnections',round(avg(connections),1),
			'averageConnectionUsagePercent',round(avg(connections/nullif(max_connections,0)*100),1),
			'averageBlockedSessions',round(avg(blocked_sessions),1),
			'averageLongTransactions',round(avg(long_transactions),1),
			'averageIdleInTransaction',round(avg(idle_in_transaction),1),
			'deadlockDelta',sum(deadlock_delta),
			'averageDatabaseBytes',round(avg(database_bytes),0),
			'averageReplicationLagBytes',round(avg(replication_lag_bytes),0),
			'averageLinuxCpuPercent',round(avg(linux_cpu),1),
			'averageLinuxMemoryPercent',round(avg(linux_memory),1),
			'transactionsPerSecond',round(avg(transactions_per_second),2),
			'walBytesPerSecond',round(avg(wal_bytes_per_second),1))
		FROM rated GROUP BY asset_id,date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz)
		ORDER BY asset_id,date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz)`, ids, from, to, bucket)
}

func (s *Postgres) queryJSON(ctx context.Context, query string, args ...any) ([]json.RawMessage, error) {
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return jsonRows(rows)
}

func (s *Postgres) RawSQLEvidence(ctx context.Context, assetID, kind, id string) (any, error) {
	var query, key string
	switch kind {
	case "slow-logs":
		query = `SELECT jsonb_build_object('id',id,'assetId',asset_id,'observedAt',observed_at,
			'durationMs',duration_ms,'queryId',query_id,'sourceFormat',source_format)
			FROM slow_log_entries WHERE asset_id=$1 AND id=$2`
		key = "log"
	case "sql-plans":
		query = `SELECT jsonb_build_object('id',id,'assetId',asset_id,'queryId',query_id,
			'observedAt',observed_at,'source',source,'shapeHash',shape_hash,'summary',summary_json)
			FROM sql_plan_snapshots WHERE asset_id=$1 AND id=$2`
		key = "plan"
	default:
		return nil, fmt.Errorf("unsupported SQL evidence kind")
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, query, assetID, id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{key: json.RawMessage(raw)}, nil
}

func timeRange(params url.Values, defaultDuration, maxDuration time.Duration) (time.Time, time.Time, error) {
	to := time.Now().UTC()
	if params.Get("to") != "" {
		parsed, err := time.Parse(time.RFC3339Nano, params.Get("to"))
		if err != nil {
			return time.Time{}, time.Time{}, ErrInvalidRange
		}
		to = parsed
	}
	from := to.Add(-defaultDuration)
	if params.Get("from") != "" {
		parsed, err := time.Parse(time.RFC3339Nano, params.Get("from"))
		if err != nil {
			return time.Time{}, time.Time{}, ErrInvalidRange
		}
		from = parsed
	} else if defaultDuration == 0 {
		return time.Time{}, time.Time{}, ErrInvalidRange
	}
	if !to.After(from) || to.Sub(from) > maxDuration {
		return time.Time{}, time.Time{}, ErrInvalidRange
	}
	return from, to, nil
}
