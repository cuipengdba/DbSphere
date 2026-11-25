package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/cuipengdba/dbsphere/internal/collector"
)

var ErrInvalidFleetQuery = errors.New("invalid fleet query")

type FleetQuery struct {
	Search, Runtime, Environment, Tag, Status string
	Major, Page, Limit, StaleSeconds          int
}

func (s *Postgres) MonitoringFleet(ctx context.Context, q FleetQuery, ids []string) (any, error) {
	if q.Page < 1 || q.Page > 1000 || q.Limit < 1 || q.Limit > 50 || len(q.Search) > 384 ||
		len(q.Environment) > 80 || len(q.Tag) > 64 || (q.Major != 0 && !collector.PostgresMajorSupported(q.Major)) ||
		(q.Runtime != "" && q.Runtime != "linux" && q.Runtime != "k8s" && q.Runtime != "managed") ||
		(q.Status != "" && q.Status != "incident" && q.Status != "coverage" && q.Status != "healthy") {
		return nil, ErrInvalidFleetQuery
	}
	var response json.RawMessage
	err := s.pool.QueryRow(ctx, monitoringFleetSQL, ids, strings.TrimSpace(q.Search), q.Runtime, q.Environment, q.Tag,
		q.Major, q.StaleSeconds, q.Status, q.Limit, (q.Page-1)*q.Limit).Scan(&response)
	return response, err
}

const monitoringFleetSQL = `WITH visible AS (
	SELECT a.id,a.name,a.runtime,a.environment,a.version_num,coalesce('pg:'||a.cluster_name,'instance:'||a.id) AS cluster_key,
		coalesce(a.cluster_name,a.name) AS cluster_name,e.first_id,e.started_at,e.total,
		CASE WHEN a.last_sample_at IS NULL THEN 'pending'
			WHEN $7::int>0 AND now()-a.last_sample_at>=make_interval(secs=>$7) THEN 'stale'
			WHEN a.last_status='ok' THEN 'ok' ELSE 'error' END AS coverage,
		CASE WHEN $7::int>0 AND now()-a.last_sample_at>=make_interval(secs=>$7) THEN 'unknown'
			WHEN a.last_status='ok' AND a.last_sample_at IS NOT NULL OR m.pg_status='ok' THEN 'available'
			WHEN m.pg_status='availability' THEN 'unavailable' ELSE 'unknown' END AS pg_state
	FROM assets a
	LEFT JOIN LATERAL (SELECT metrics_json#>>'{collector,pgStatus}' AS pg_status FROM samples
		WHERE a.last_status<>'ok' AND asset_id=a.id AND collected_at=a.last_sample_at ORDER BY id DESC LIMIT 1) m ON true
	LEFT JOIN LATERAL (SELECT count(*)::int AS total,min(k.started_at) AS started_at,
		(array_agg(k.id ORDER BY k.started_at,k.id))[1] AS first_id
		FROM open_event_groups g JOIN event_keys k ON k.id=g.event_id WHERE g.asset_id=a.id) e ON true
	WHERE a.runtime IN ('linux','k8s','managed') AND ($1::uuid[] IS NULL OR a.id=ANY($1))
	AND ($2='' OR strpos(lower(a.name),lower($2))>0 OR strpos(lower(coalesce(a.cluster_name,'')),lower($2))>0)
	AND ($3='' OR a.runtime=$3) AND ($4='' OR a.environment=$4) AND ($5='' OR $5=ANY(a.tags))
	AND ($6::int=0 OR a.version_num/10000=$6)
), grouped AS (
	SELECT cluster_key,cluster_name,count(*)::int AS members,
		count(*) FILTER (WHERE pg_state='available')::int AS available,
		count(*) FILTER (WHERE pg_state='unavailable')::int AS unavailable,
		count(*) FILTER (WHERE coverage='ok')::int AS observed,
		count(*) FILTER (WHERE coverage='stale')::int AS stale,
		count(*) FILTER (WHERE coverage='pending')::int AS pending,
		count(*) FILTER (WHERE coverage='error')::int AS failed,
		array_agg(DISTINCT runtime ORDER BY runtime) AS runtimes,
		array_agg(DISTINCT version_num/10000 ORDER BY version_num/10000) FILTER (WHERE version_num IS NOT NULL) AS versions,
		min(started_at) AS started_at,sum(total)::int AS events,
		(array_agg(first_id ORDER BY started_at,id) FILTER (WHERE first_id IS NOT NULL))[1] AS first_event,
		(jsonb_agg(jsonb_build_object('id',id,'name',name,'pgState',pg_state,'coverage',coverage)
			ORDER BY (pg_state='unavailable') DESC,(total>0) DESC,(coverage<>'ok') DESC,name,id)
			FILTER (WHERE total>0 OR coverage<>'ok' OR pg_state='unavailable'))->0 AS affected
	FROM visible GROUP BY cluster_key,cluster_name
), ranked AS (
	SELECT *,CASE WHEN unavailable>0 OR events>0 THEN 'incident'
		WHEN stale+pending+failed>0 THEN 'coverage' ELSE 'healthy' END AS status
	FROM grouped
), filtered AS (
	SELECT * FROM ranked WHERE $8='' OR status=$8
), page AS (
	SELECT * FROM filtered ORDER BY (status='incident') DESC,(status='coverage') DESC,started_at NULLS LAST,cluster_key
	LIMIT $9 OFFSET $10
)
SELECT jsonb_build_object('total',(SELECT count(*) FROM filtered),'summary',(SELECT jsonb_build_object(
	'clusters',count(*),'incidents',count(*) FILTER (WHERE status='incident'),'members',coalesce(sum(members),0),
	'observed',coalesce(sum(observed),0),'stale',coalesce(sum(stale),0),
	'pending',coalesce(sum(pending),0),'failed',coalesce(sum(failed),0),'events',coalesce(sum(events),0)) FROM filtered),
	'clusters',coalesce((SELECT jsonb_agg(to_jsonb(page) ORDER BY (status='incident') DESC,(status='coverage') DESC,started_at NULLS LAST,cluster_key) FROM page),'[]'::jsonb))`
