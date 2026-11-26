package store

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

func (s *Postgres) pgPerformanceHistory(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	minutes, bucketErr := strconv.Atoi(params.Get("bucketMinutes"))
	if err != nil || params.Get("to") == "" || bucketErr != nil ||
		(minutes != 1 && minutes != 5 && minutes != 15 && minutes != 60) || to.Sub(from) > time.Duration(400*minutes)*time.Minute {
		return nil, ErrInvalidRange
	}
	rows, err := s.queryJSON(ctx, pgPerformanceHistorySQL, id, from, to, minutes)
	return map[string]any{"series": rows}, err
}

// Select an actual sample per signal and bucket. Event links require the firing
// evidence to name that exact sample and signal on the same asset.
const pgPerformanceHistorySQL = `WITH observed AS (
	SELECT id,collected_at,metrics_json AS m,
		date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz) AS bucket
	FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
	AND (status='ok' OR metrics_json#>>'{collector,pgStatus}'='ok')
), readings AS (
	SELECT id,collected_at,bucket,t.signal,t.value,
		CASE WHEN t.signal='pg.sql_regression_ratio' THEN m#>>'{sqlRegression,worst,queryId}' END AS query_id
	FROM observed CROSS JOIN LATERAL (VALUES
		('pg.blocked_sessions',CASE WHEN m#>>'{diagnostics,locks,status}'='ok'
			THEN m#>'{diagnostics,locks,data,blockedSessions}' END),
		('pg.connection_utilization',CASE WHEN jsonb_typeof(m->'connections')='number'
			AND jsonb_typeof(m->'max_connections')='number'
			THEN CASE WHEN (m->>'max_connections')::numeric>0
				THEN to_jsonb(100*(m->>'connections')::numeric/(m->>'max_connections')::numeric) END END),
		('pg.sql_regression_ratio',CASE WHEN m#>>'{sqlRegression,status}'='ok'
			THEN m#>'{sqlRegression,maxRatio}' END)
	) AS t(signal,value) WHERE jsonb_typeof(t.value)='number'
), triggering AS MATERIALIZED (
	SELECT DISTINCT ON (ev.payload_json->>'sampleId',ev.payload_json->>'metric')
		ev.payload_json->>'sampleId' AS sample_id,ev.payload_json->>'metric' AS signal,e.id AS event_id
	FROM evidence ev JOIN events e ON e.id=ev.event_id
	WHERE e.asset_id=$1 AND ev.observed_at>=$2 AND ev.observed_at<$3
	AND ev.kind IN ('alert_firing','alert_unsuppressed')
	AND ev.payload_json->>'metric' IN ('pg.blocked_sessions','pg.connection_utilization','pg.sql_regression_ratio')
	ORDER BY ev.payload_json->>'sampleId',ev.payload_json->>'metric',ev.observed_at,e.id
), points AS (
	SELECT r.*,t.event_id FROM readings r LEFT JOIN triggering t ON t.sample_id=r.id::text AND t.signal=r.signal
), peaks AS (
	SELECT DISTINCT ON (bucket,signal) * FROM points
	ORDER BY bucket,signal,value DESC,(event_id IS NULL),collected_at DESC,id DESC
), triggers AS (
	SELECT DISTINCT ON (bucket,signal) bucket,signal,collected_at,id,event_id FROM points
	WHERE event_id IS NOT NULL ORDER BY bucket,signal,collected_at DESC,id DESC
)
SELECT jsonb_build_object('bucket',p.bucket,'sampleId',p.id,'at',p.collected_at,'signal',p.signal,
	'value',p.value,'queryId',p.query_id,'eventId',p.event_id,
	'trigger',CASE WHEN t.id<>p.id THEN jsonb_build_object('sampleId',t.id,'at',t.collected_at,'eventId',t.event_id) END)
FROM peaks p LEFT JOIN triggers t USING(bucket,signal) ORDER BY p.bucket,p.signal`
