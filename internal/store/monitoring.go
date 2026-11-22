package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

const monitoringInstancesSQL = `SELECT jsonb_build_object(
	'assetId',a.id,'openEvents',e.total,
	'postgresState',CASE WHEN m.status='ok' OR m.metrics_json#>>'{collector,pgStatus}'='ok' THEN 'available'
		WHEN m.metrics_json#>>'{collector,pgStatus}'='availability' THEN 'unavailable' ELSE 'unknown' END,
	'replicationLagBytes',m.metrics_json->'replication_lag_bytes',
	'connections',m.metrics_json->'connections','maxConnections',m.metrics_json->'max_connections')
	FROM assets a LEFT JOIN LATERAL (
		SELECT status,metrics_json FROM samples WHERE asset_id=a.id AND collected_at=a.last_sample_at
		ORDER BY id DESC LIMIT 1
	) m ON true LEFT JOIN LATERAL (
		SELECT count(*)::int AS total FROM open_event_groups WHERE asset_id=a.id
	) e ON true /*asset_scope*/ ORDER BY a.id`

func monitoringBucketLimit(minutes int) time.Duration {
	if minutes == 60 {
		return 30 * 24 * time.Hour
	}
	return time.Duration(minutes*400) * time.Minute
}

// MonitoringSeries reads one bounded set of time buckets for authorized assets.
func (s *Postgres) MonitoringSeries(ctx context.Context, ids []string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 30*24*time.Hour)
	minutes, parseErr := strconv.Atoi(params.Get("bucketMinutes"))
	if err != nil || parseErr != nil || params.Get("to") == "" || len(ids) < 1 || len(ids) > 5 ||
		(minutes != 1 && minutes != 5 && minutes != 15 && minutes != 60) ||
		to.Sub(from) > monitoringBucketLimit(minutes) {
		return nil, ErrInvalidRange
	}
	values, err := s.readMonitoringBuckets(ctx, ids, from, to, minutes)
	if err != nil {
		return nil, err
	}
	events, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',e.id,'assetId',e.asset_id,
		'assetName',a.name,'title',e.title,'status',e.status,'startedAt',e.started_at,'endedAt',e.ended_at)
		FROM events e JOIN assets a ON a.id=e.asset_id WHERE e.asset_id=ANY($1::uuid[])
		AND e.started_at<$3 AND (e.ended_at IS NULL OR e.ended_at >= $2)
		ORDER BY (e.status='open') DESC,e.started_at DESC,e.id LIMIT 40`, ids, from, to)
	return map[string]any{"series": values, "events": events}, err
}

func (s *Postgres) monitoringContext(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	if err != nil {
		return nil, err
	}
	var snapshot json.RawMessage
	err = s.pool.QueryRow(ctx, `SELECT jsonb_build_object(
		'id',id,'collectedAt',collected_at,'status',status,
		'role',metrics_json->'role','locks',metrics_json#>'{diagnostics,lockChain}',
		'activity',metrics_json#>'{diagnostics,activity}',
		'blocked',metrics_json#>'{diagnostics,locks}',
		'transactions',metrics_json#>'{diagnostics,transactions}',
		'linux',metrics_json->'linux','k8s',metrics_json->'k8s','routing',metrics_json->'routing')
		FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
		ORDER BY collected_at DESC,id DESC LIMIT 1`, id, from, to).Scan(&snapshot)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var blockingSample json.RawMessage
	if params.Get("blocking") == "true" {
		err = s.pool.QueryRow(ctx, `SELECT jsonb_build_object('id',id,'collectedAt',collected_at,
			'locks',metrics_json#>'{diagnostics,lockChain}')
			FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
			AND metrics_json#>>'{diagnostics,lockChain,status}'='ok'
			AND CASE WHEN jsonb_typeof(metrics_json#>'{diagnostics,lockChain,data}')='array'
			  THEN jsonb_array_length(metrics_json#>'{diagnostics,lockChain,data}') > 0 ELSE false END
			ORDER BY collected_at DESC,id DESC LIMIT 1`, id, from, to).Scan(&blockingSample)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	events, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',id,'title',title,'status',status,
		'startedAt',started_at,'endedAt',ended_at) FROM events WHERE asset_id=$1 AND started_at<$3
		AND (ended_at IS NULL OR ended_at >= $2) ORDER BY (status='open') DESC,started_at DESC LIMIT 12`, id, from, to)
	return map[string]any{"snapshot": snapshot, "blockingSample": blockingSample, "events": events}, err
}

func (s *Postgres) waitHistory(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	minutes, parseErr := strconv.Atoi(params.Get("bucketMinutes"))
	if err != nil || parseErr != nil || (minutes != 1 && minutes != 5 && minutes != 15 && minutes != 60) ||
		to.Sub(from) > time.Duration(minutes*400)*time.Minute {
		return nil, ErrInvalidRange
	}
	depth, ok := map[string]int{"type": 3, "event": 1, "query": 0}[params.Get("group")]
	waitType, waitEvent := params.Get("waitType"), params.Get("waitEvent")
	if !ok || len(waitType) > 64 || len(waitEvent) > 128 {
		return nil, ErrInvalidRange
	}
	values, err := s.queryJSON(ctx, waitHistorySQL, id, from, to, minutes, depth, waitType, waitEvent)
	return map[string]any{"buckets": values}, err
}

const waitHistorySQL = `WITH observed AS (
	SELECT date_bin(make_interval(mins=>$4),collected_at,'2000-01-01 00:00:00+00'::timestamptz) AS bucket,
		CASE WHEN (status='ok' OR metrics_json#>>'{collector,pgStatus}'='ok')
			AND metrics_json#>>'{diagnostics,activeWaits,status}'='ok'
			AND jsonb_typeof(metrics_json#>'{diagnostics,activeWaits,data,groups}')='array'
			THEN metrics_json#>'{diagnostics,activeWaits,data}' END AS waits
	FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
), bins AS (
	SELECT bucket,count(*)::int AS samples,count(waits)::int AS observed,
		coalesce(bool_or(waits->>'truncated'='true'),false) AS truncated
	FROM observed GROUP BY bucket
), grouped AS (
	SELECT o.bucket,w."waitType",w."waitEvent",w."queryId",sum(w.sessions) AS sessions
	FROM observed o CROSS JOIN LATERAL jsonb_to_recordset(o.waits->'groups')
		AS w(depth int,"waitType" text,"waitEvent" text,"queryId" text,sessions int)
	WHERE w.depth=$5 AND ($6='' OR w."waitType"=$6) AND ($7='' OR w."waitEvent"=$7)
	GROUP BY o.bucket,w."waitType",w."waitEvent",w."queryId"
), top_groups AS (
	SELECT "waitType","waitEvent","queryId" FROM grouped GROUP BY 1,2,3
	ORDER BY sum(sessions) DESC,1,2,3 LIMIT 12
), assigned AS (
	SELECT g.bucket,t."waitType",t."waitEvent",t."queryId",(t."waitType" IS NULL) AS other,
		sum(g.sessions) AS sessions
	FROM grouped g LEFT JOIN top_groups t ON g."waitType"=t."waitType"
		AND g."waitEvent" IS NOT DISTINCT FROM t."waitEvent" AND g."queryId" IS NOT DISTINCT FROM t."queryId"
	GROUP BY 1,2,3,4
), display AS (
	SELECT a.bucket,jsonb_agg(jsonb_build_object('waitType',a."waitType",'waitEvent',a."waitEvent",'queryId',a."queryId",
		'other',a.other,'averageSessions',round(a.sessions/nullif(b.observed,0),3))
		ORDER BY a.other,a."waitType",a."waitEvent",a."queryId") AS groups
	FROM assigned a JOIN bins b ON b.bucket=a.bucket GROUP BY a.bucket
)
SELECT jsonb_build_object('bucket',b.bucket,'samples',b.samples,'observed',b.observed,
	'truncated',b.truncated,'groups',coalesce(d.groups,'[]'::jsonb))
FROM bins b LEFT JOIN display d ON d.bucket=b.bucket ORDER BY b.bucket`
