package store

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

func (s *Postgres) lockHistory(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	if err != nil || params.Get("to") == "" {
		return nil, ErrInvalidRange
	}
	sample := params.Get("sample")
	if sample != "" && !evidenceUUID.MatchString(sample) {
		return nil, ErrInvalidRange
	}
	values, err := s.queryJSON(ctx, `WITH matching AS (
		SELECT id,collected_at,metrics_json#>'{diagnostics,lockChain}' AS locks
		FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
		AND metrics_json#>>'{diagnostics,lockChain,status}'='ok'
		AND jsonb_typeof(metrics_json#>'{diagnostics,lockChain,data}')='array'
	), selected AS (
		(SELECT * FROM matching WHERE jsonb_array_length(locks->'data')>0 ORDER BY collected_at DESC,id DESC LIMIT 60)
		UNION
		(SELECT * FROM matching ORDER BY collected_at DESC,id DESC LIMIT 1)
		UNION
		SELECT * FROM matching WHERE id::text=$4
	)
	SELECT jsonb_build_object('id',id,'collectedAt',collected_at,'locks',locks)
	FROM selected ORDER BY collected_at DESC,id DESC`, id, from, to, sample)
	return map[string]any{"samples": values}, err
}

func (s *Postgres) queryTraceHistory(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	queryID := params.Get("queryId")
	if _, idErr := strconv.ParseInt(queryID, 10, 64); err != nil || idErr != nil || params.Get("to") == "" {
		return nil, ErrInvalidRange
	}
	values, err := s.queryJSON(ctx, `SELECT jsonb_build_object('traceId',trace_id,'firstAt',min(started_at),
		'lastAt',max(ended_at),'databaseSpans',count(*),'errors',count(*) FILTER (WHERE status='error'),
		'services',array_agg(DISTINCT service ORDER BY service))
		FROM trace_spans WHERE asset_id=$1 AND started_at>=$2 AND started_at<$3 AND query_id=$4 AND kind='postgresql_client'
		GROUP BY trace_id ORDER BY min(started_at) DESC,trace_id LIMIT 40`, id, from, to, queryID)
	return map[string]any{"traces": values}, err
}
