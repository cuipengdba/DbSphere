package store

import (
	"context"
	"encoding/json"
	"time"
)

type OverviewQuery struct {
	Organization, Environment, Cluster string
	From, To                           time.Time
	InspectFrom, InspectTo             *time.Time
	StaleSeconds, Offset               int
}

func (s *Postgres) MonitoringOverview(ctx context.Context, ids []string, q OverviewQuery) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var result json.RawMessage
	err := s.pool.QueryRow(ctx, monitoringOverviewSQL, ids, OrganizationFromContext(ctx), q.Organization, q.Environment,
		q.Cluster, q.StaleSeconds, q.From, q.To, q.Offset, q.InspectFrom, q.InspectTo).Scan(&result)
	return result, err
}

const monitoringOverviewSQL = `WITH visible AS MATERIALIZED (
	SELECT a.id,a.name,a.runtime,a.organization_id,o.name AS organization,a.environment,a.cluster_name,
		a.last_sample_at,a.last_status,a.role,a.linux_host,a.k8s_node
	FROM assets a JOIN organizations o ON o.id=a.organization_id
	WHERE ($1::uuid[] IS NULL OR a.id=ANY($1)) AND ($2='' OR a.organization_id::text=$2)
), selected AS MATERIALIZED (
	SELECT * FROM visible WHERE ($3='' OR organization_id::text=$3)
		AND ($4='' OR environment=$4) AND ($5='' OR cluster_name=$5)
), open_events AS MATERIALIZED (
	SELECT e.id,e.started_at,e.asset_id,e.title,e.event_group,s.name AS asset_name
	FROM open_event_groups g JOIN selected s ON s.id=g.asset_id
	JOIN event_keys k ON k.id=g.event_id JOIN events e ON e.id=k.id AND e.started_at=k.started_at
	WHERE e.status='open'
), event_by_asset AS (
	SELECT asset_id,count(*) AS total,bool_or(event_group='availability') AS unavailable
	FROM open_events GROUP BY asset_id
), health AS MATERIALIZED (
	SELECT s.*,coalesce(e.total,0) AS events,
		CASE WHEN last_sample_at IS NULL THEN 'pending'
			WHEN $6::int>0 AND last_sample_at<=now()-make_interval(secs=>$6) THEN 'stale'
			WHEN last_status='ok' THEN 'ok' ELSE 'error' END AS freshness,
		coalesce(e.unavailable,false) AS unavailable
	FROM selected s LEFT JOIN event_by_asset e ON e.asset_id=s.id
), ranked AS (
	SELECT *,CASE WHEN unavailable THEN 0 WHEN events>0 THEN 1 WHEN freshness<>'ok' THEN 2 ELSE 3 END AS priority
	FROM health
), page AS (
	SELECT * FROM ranked ORDER BY priority,organization_id,environment,cluster_name,name,id LIMIT 36 OFFSET $9
), lifecycle AS MATERIALIZED (
	SELECT e.id,e.asset_id,s.name AS asset_name,e.title,e.started_at,e.ended_at,e.acknowledged_at,e.event_group
	FROM events e JOIN selected s ON s.id=e.asset_id
	WHERE e.started_at<$8 AND (e.ended_at IS NULL OR e.ended_at>=$7)
), maintenance AS MATERIALIZED (
	SELECT c.id,c.asset_id,s.name AS asset_name,c.label,c.occurred_at FROM asset_changes c JOIN selected s ON s.id=c.asset_id
	WHERE c.kind='maintenance' AND c.occurred_at>=$7 AND c.occurred_at<$8
), bins AS (
	SELECT n,$7::timestamptz+($8::timestamptz-$7::timestamptz)*(n/36.0) AS start,
		$7::timestamptz+($8::timestamptz-$7::timestamptz)*((n+1)/36.0) AS finish
	FROM generate_series(0,35) n
), signals AS (
	SELECT b.n,b.start,
		count(*) FILTER (WHERE l.started_at>=b.start AND l.started_at<b.finish) AS opened,
		count(*) FILTER (WHERE l.ended_at>=b.start AND l.ended_at<b.finish) AS recovered,
		count(*) FILTER (WHERE l.event_group='availability' AND (l.ended_at IS NULL OR l.ended_at>b.start)) AS availability,
		(SELECT count(*) FROM maintenance m WHERE m.occurred_at>=b.start AND m.occurred_at<b.finish) AS maintenance
	FROM bins b LEFT JOIN lifecycle l ON l.started_at<b.finish AND (l.ended_at IS NULL OR l.ended_at>=b.start)
	GROUP BY b.n,b.start,b.finish
), inspection AS (
	SELECT x.* FROM (
		SELECT l.id,l.asset_id,l.asset_name,l.title,l.event_group AS kind,p.phase,p.at,l.started_at,l.ended_at
		FROM lifecycle l CROSS JOIN LATERAL (VALUES ('opened',l.started_at),('confirmed',l.acknowledged_at),('recovered',l.ended_at),
			('ongoing',CASE WHEN l.started_at<$10::timestamptz AND (l.ended_at IS NULL OR l.ended_at>=$11::timestamptz)
			THEN $10::timestamptz END)) p(phase,at)
		WHERE p.at>=$10 AND p.at<$11
		UNION ALL
		SELECT m.id,m.asset_id,m.asset_name,m.label,'maintenance','maintenance',m.occurred_at,m.occurred_at,NULL::timestamptz
		FROM maintenance m WHERE m.occurred_at>=$10 AND m.occurred_at<$11
	) x ORDER BY at DESC,phase,id LIMIT 25
)
SELECT jsonb_build_object(
	'from',$7::timestamptz,'to',$8::timestamptz,'observedAt',now(),
	'summary',(SELECT jsonb_build_object('clusters',count(DISTINCT (organization_id,environment,coalesce(cluster_name,id::text))),
		'assets',count(*),'fresh',count(*) FILTER(WHERE freshness='ok'),
		'unavailable',count(*) FILTER(WHERE unavailable),'stale',count(*) FILTER(WHERE freshness IN ('pending','stale')),
		'events',coalesce(sum(events),0),'linux',count(*) FILTER(WHERE runtime='linux' OR linux_host IS NOT NULL),
		'kubernetes',count(*) FILTER(WHERE runtime='k8s')) FROM health),
	'organizations',coalesce((SELECT jsonb_agg(row ORDER BY row.name,row.id)
		FROM (SELECT DISTINCT organization_id AS id,organization AS name FROM visible) row),'[]'),
	'environments',coalesce((SELECT jsonb_agg(value ORDER BY value)
		FROM (SELECT DISTINCT environment AS value FROM visible WHERE environment IS NOT NULL
		AND ($3='' OR organization_id::text=$3)) choices),'[]'),
	'clusters',coalesce((SELECT jsonb_agg(value ORDER BY value)
		FROM (SELECT DISTINCT cluster_name AS value FROM visible WHERE cluster_name IS NOT NULL
		AND ($3='' OR organization_id::text=$3) AND ($4='' OR environment=$4)) choices),'[]'),
	'assets',coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'name',name,'runtime',runtime,'role',role,
		'organizationId',organization_id,'organization',organization,'environment',environment,'cluster',cluster_name,'sampleAt',last_sample_at,
		'freshness',freshness,'unavailable',unavailable,'events',events)
		ORDER BY priority,organization_id,environment,cluster_name,name,id) FROM page),'[]'),
	'alerts',coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'assetId',asset_id,'assetName',asset_name,
		'title',title,'group',event_group,'startedAt',started_at) ORDER BY (event_group='availability') DESC,started_at,id)
		FROM (SELECT * FROM open_events ORDER BY (event_group='availability') DESC,started_at,id LIMIT 8) alerts),'[]'),
	'timeline',(SELECT jsonb_agg(to_jsonb(signals)-'n' ORDER BY n) FROM signals),
	'inspection',CASE WHEN $10::timestamptz IS NOT NULL THEN (SELECT jsonb_build_object(
		'from',$10,'to',$11::timestamptz,'limited',count(*)>24,
		'rows',coalesce(jsonb_agg(jsonb_build_object('id',i.id,'assetId',i.asset_id,'assetName',i.asset_name,
			'title',i.title,'kind',i.kind,'phase',i.phase,'at',i.at,'startedAt',i.started_at,'endedAt',i.ended_at,
			'severity',priority.severity) ORDER BY i.at DESC,i.phase,i.id),'[]'))
		FROM inspection i LEFT JOIN LATERAL (
			SELECT f.payload_json->>'severity' AS severity FROM evidence f
			WHERE i.phase<>'maintenance' AND f.event_id=i.id AND f.kind='alert_firing'
			AND f.payload_json->>'severity' IN ('critical','warning','info')
			ORDER BY CASE f.payload_json->>'severity' WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,f.observed_at LIMIT 1
		) priority ON true) END)`
