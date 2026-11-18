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
		return s.SQLSummary(ctx, id)
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
			ORDER BY l.observed_at DESC LIMIT 100`, id, from, to)
		return map[string]any{"entries": values}, err
	case "sql-plans":
		values, err := s.queryJSON(ctx, `SELECT jsonb_build_object(
			'id',p.id,'assetId',p.asset_id,'queryId',p.query_id,
			'observedAt',p.observed_at,'source',p.source,'shapeHash',p.shape_hash,
			'summary',p.summary_json,
			'eventIds',coalesce((SELECT jsonb_agg(e.id) FROM events e
				WHERE e.asset_id=p.asset_id AND e.started_at<=p.observed_at+interval '5 minutes'
				AND (e.ended_at IS NULL OR e.ended_at>=p.observed_at-interval '5 minutes')),'[]'::jsonb))
			FROM sql_plan_snapshots p WHERE p.asset_id=$1 ORDER BY p.observed_at DESC LIMIT 50`, id)
		return map[string]any{"plans": values}, err
	case "series":
		if params.Get("from") == "" || params.Get("to") == "" {
			return nil, ErrInvalidRange
		}
		from, to, err := timeRange(params, 0, 90*24*time.Hour)
		if err != nil {
			return nil, err
		}
		bucket := 1
		if params.Get("bucketHours") != "" {
			bucket, err = strconv.Atoi(params.Get("bucketHours"))
			if err != nil || bucket < 1 || bucket > 24 {
				return nil, ErrInvalidRange
			}
		}
		values, err := s.queryJSON(ctx, `SELECT jsonb_build_object(
			'bucket',date_bin(make_interval(hours=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz),
			'samples',count(*)::int,'errors',count(*) FILTER (WHERE status='error')::int,
			'averageConnections',round(avg(CASE WHEN jsonb_typeof(metrics_json->'connections')='number'
				THEN (metrics_json->>'connections')::numeric END),1))
			FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
			GROUP BY date_bin(make_interval(hours=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz)
			ORDER BY date_bin(make_interval(hours=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz)`, id, from, to, bucket)
		return map[string]any{"series": values}, err
	case "capacity":
		forecast, err := s.CapacityForecast(ctx, id)
		return map[string]any{"forecast": forecast}, err
	default:
		return nil, fmt.Errorf("unsupported asset section")
	}
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
