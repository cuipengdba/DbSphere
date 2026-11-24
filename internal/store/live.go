package store

import (
	"context"
	"encoding/json"
	"time"
)

type LiveScope struct {
	Organization, Environment, Cluster, Engine string
}

// LiveObservation applies the authorized scope before ranking or limiting assets.
func (s *Postgres) LiveObservation(ctx context.Context, ids []string, staleAfter int, scope LiveScope) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var result json.RawMessage
	err := s.pool.QueryRow(ctx, liveObservationSQL, ids, OrganizationFromContext(ctx), staleAfter,
		scope.Organization, scope.Environment, scope.Cluster, scope.Engine).Scan(&result)
	return result, err
}

const liveObservationSQL = `WITH visible AS MATERIALIZED (
	SELECT a.id,a.name,a.cluster_name,a.runtime,a.environment,a.organization_id,o.name AS organization_name,
		CASE WHEN a.runtime='mysql' THEN 'mysql' ELSE 'postgres' END AS engine,
		a.role,a.k8s_cluster,a.k8s_namespace,a.k8s_pod,a.k8s_node,a.last_sample_at,a.last_status,
		CASE WHEN a.last_sample_at IS NULL THEN 'pending'
			WHEN $3::int>0 AND a.last_sample_at<=now()-make_interval(secs=>$3) THEN 'stale'
			ELSE a.last_status END AS freshness
	FROM assets a JOIN organizations o ON o.id=a.organization_id
	WHERE ($1::uuid[] IS NULL OR a.id=ANY($1)) AND (nullif($2,'') IS NULL OR a.organization_id=nullif($2,'')::uuid)
), inventory AS MATERIALIZED (
	SELECT *,CASE WHEN cluster_name IS NULL OR cluster_name='' THEN id::text
		ELSE organization_id::text || '/' || engine || '/' || cluster_name END AS cluster_key FROM visible
), selected AS MATERIALIZED (
	SELECT * FROM inventory WHERE ($4='' OR organization_id::text=$4) AND ($5='' OR environment=$5)
		AND ($6='' OR cluster_key=$6) AND ($7='' OR engine=$7)
), active AS MATERIALIZED (
	SELECT e.id,e.kind,e.event_group,e.title,e.status,v.id AS asset_id,v.name AS asset_name,v.organization_id,e.started_at,e.ended_at,
		priority.severity
	FROM open_event_groups g JOIN selected v ON v.id=g.asset_id JOIN event_keys k ON k.id=g.event_id
	JOIN events e ON e.id=k.id AND e.started_at=k.started_at AND e.status='open'
	LEFT JOIN LATERAL (
		SELECT payload_json->>'severity' AS severity FROM evidence f WHERE f.event_id=e.id AND f.kind='alert_firing'
		AND f.payload_json->>'severity' IN ('critical','warning','info')
		ORDER BY CASE f.payload_json->>'severity' WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,f.observed_at LIMIT 1
	) priority ON true
), counts AS (
	SELECT asset_id,count(*) AS total,bool_or(event_group='availability') AS unavailable FROM active GROUP BY asset_id
), shown AS (
	SELECT v.*,coalesce(c.total,0) AS open_events,coalesce(c.unavailable,false) AS unavailable
	FROM selected v LEFT JOIN counts c ON c.asset_id=v.id
	ORDER BY coalesce(c.unavailable,false) DESC,(c.total IS NOT NULL) DESC,(v.freshness='error') DESC,v.last_sample_at DESC NULLS LAST,v.id LIMIT 120
), readings AS (
	SELECT jsonb_build_object('id',a.id,'name',a.name,'cluster',a.cluster_name,'runtime',a.runtime,
		'organizationId',a.organization_id,'organization',a.organization_name,'environment',a.environment,'engine',a.engine,
		'role',a.role,'pod',a.k8s_pod,'namespace',a.k8s_namespace,'node',a.k8s_node,'k8sCluster',a.k8s_cluster,
		'collectedAt',a.last_sample_at,'sampleId',m.id,'freshness',a.freshness,'openEvents',a.open_events,'unavailable',a.unavailable,
		'connections',m.metrics_json->'connections','maxConnections',m.metrics_json->'max_connections',
		'active',CASE WHEN m.metrics_json#>>'{diagnostics,instanceSessions,status}'='ok' THEN m.metrics_json#>'{diagnostics,instanceSessions,data,active}' END,
		'poolWaiting',CASE WHEN m.metrics_json#>>'{pgbouncer,status}'='ok' AND jsonb_typeof(m.metrics_json#>'{pgbouncer,pools}')='array'
			THEN (SELECT sum((p->>'clientWaiting')::numeric) FROM jsonb_array_elements(m.metrics_json#>'{pgbouncer,pools}') p
				WHERE jsonb_typeof(p->'clientWaiting')='number') END,
		'replicationLag',m.metrics_json->'replication_lag_bytes',
		'ioPressure',m.metrics_json#>'{containerStorage,containers}'->(m.metrics_json#>>'{containerStorage,primary}')#>'{pressure,io_some_avg10}',
		'blocked',CASE WHEN m.metrics_json#>>'{diagnostics,locks,status}'='ok'
			THEN m.metrics_json#>'{diagnostics,locks,data,blockedSessions}' END,
		'linux',m.metrics_json#>'{linux,status}','k8s',m.metrics_json#>'{k8s,status}') AS value
	FROM shown a LEFT JOIN LATERAL (SELECT id,metrics_json FROM samples
		WHERE asset_id=a.id AND collected_at=a.last_sample_at ORDER BY id DESC LIMIT 1) m ON true
), recent AS (
	SELECT *,CASE WHEN severity='critical' THEN 0 WHEN event_group='availability' THEN 1 WHEN severity='warning' THEN 2 ELSE 3 END AS priority FROM (
		SELECT * FROM active
		UNION ALL
		SELECT e.id,e.kind,e.event_group,e.title,e.status,e.asset_id,v.name,v.organization_id,e.started_at,e.ended_at,NULL::text
		FROM events e JOIN selected v ON v.id=e.asset_id WHERE e.status='resolved'
		AND e.started_at>=now()-interval '7 days' AND e.ended_at>=now()-interval '6 hours'
	) rows ORDER BY (status='open') DESC,priority,coalesce(ended_at,started_at) DESC,id LIMIT 24
)
SELECT jsonb_build_object('observedAt',now(),'staleAfter',$3,
	'total',(SELECT count(*) FROM selected),'openEvents',(SELECT count(*) FROM active),
	'fresh',(SELECT count(*) FROM selected WHERE freshness='ok'),
	'unavailable',(SELECT count(*) FROM counts WHERE unavailable),
	'filters',coalesce((SELECT jsonb_agg(to_jsonb(options)) FROM (SELECT DISTINCT organization_id AS "organizationId",
		organization_name AS organization,environment,engine,cluster_key AS "clusterKey",coalesce(nullif(cluster_name,''),name) AS cluster
		FROM inventory) options),'[]'::jsonb),
	'assets',coalesce((SELECT jsonb_agg(value) FROM readings),'[]'::jsonb),
	'events',coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'kind',kind,'title',title,
		'status',status,'assetId',asset_id,'assetName',asset_name,'organizationId',organization_id,'severity',severity,'group',event_group,
		'startedAt',started_at,'endedAt',ended_at) ORDER BY (status='open') DESC,priority,coalesce(ended_at,started_at) DESC,id) FROM recent),'[]'::jsonb))`
