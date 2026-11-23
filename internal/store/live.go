package store

import (
	"context"
	"encoding/json"
	"time"
)

// LiveObservation uses the same asset permissions and persisted events as the console.
func (s *Postgres) LiveObservation(ctx context.Context, ids []string, staleAfter int) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var result json.RawMessage
	err := s.pool.QueryRow(ctx, liveObservationSQL, ids, OrganizationFromContext(ctx), staleAfter).Scan(&result)
	return result, err
}

const liveObservationSQL = `WITH visible AS MATERIALIZED (
	SELECT a.id,a.name,a.cluster_name,a.runtime,a.organization_id,o.name AS organization_name,
		a.role,a.k8s_cluster,a.k8s_namespace,a.k8s_pod,a.k8s_node,a.last_sample_at,a.last_status,
		CASE WHEN a.last_sample_at IS NULL THEN 'pending'
			WHEN $3::int>0 AND a.last_sample_at<=now()-make_interval(secs=>$3) THEN 'stale'
			ELSE a.last_status END AS freshness
	FROM assets a JOIN organizations o ON o.id=a.organization_id
	WHERE ($1::uuid[] IS NULL OR a.id=ANY($1)) AND (nullif($2,'') IS NULL OR a.organization_id=nullif($2,'')::uuid)
), active AS MATERIALIZED (
	SELECT g.event_id AS id,v.id AS asset_id,v.name AS asset_name,v.organization_id,k.started_at
	FROM open_event_groups g JOIN visible v ON v.id=g.asset_id JOIN event_keys k ON k.id=g.event_id
), counts AS (
	SELECT asset_id,count(*) AS total FROM active GROUP BY asset_id
), shown AS (
	SELECT v.*,coalesce(c.total,0) AS open_events FROM visible v LEFT JOIN counts c ON c.asset_id=v.id
	ORDER BY (c.total IS NOT NULL) DESC,(v.freshness='error') DESC,v.last_sample_at DESC NULLS LAST,v.id LIMIT 120
), readings AS (
	SELECT jsonb_build_object('id',a.id,'name',a.name,'cluster',a.cluster_name,'runtime',a.runtime,
		'organizationId',a.organization_id,'organization',a.organization_name,
		'engine',CASE WHEN a.runtime='mysql' THEN 'mysql' ELSE 'postgres' END,
		'role',a.role,'pod',a.k8s_pod,'namespace',a.k8s_namespace,'node',a.k8s_node,'k8sCluster',a.k8s_cluster,
		'collectedAt',a.last_sample_at,'sampleId',m.id,'freshness',a.freshness,'openEvents',a.open_events,
		'connections',m.metrics_json->'connections','maxConnections',m.metrics_json->'max_connections',
		'blocked',CASE WHEN m.metrics_json#>>'{diagnostics,locks,status}'='ok'
			THEN m.metrics_json#>'{diagnostics,locks,data,blockedSessions}' END,
		'linux',m.metrics_json#>'{linux,status}','k8s',m.metrics_json#>'{k8s,status}') AS value
	FROM shown a LEFT JOIN LATERAL (SELECT id,metrics_json FROM samples
		WHERE asset_id=a.id AND collected_at=a.last_sample_at ORDER BY id DESC LIMIT 1) m ON true
), recent AS (
	SELECT * FROM (
		SELECT e.id,e.kind,e.title,e.status,e.asset_id,v.name AS asset_name,v.organization_id,e.started_at,e.ended_at
		FROM (SELECT * FROM active ORDER BY started_at DESC,id LIMIT 24) v_open
		JOIN events e ON e.id=v_open.id AND e.started_at=v_open.started_at
		JOIN visible v ON v.id=e.asset_id
		UNION ALL
		SELECT e.id,e.kind,e.title,e.status,e.asset_id,v.name,v.organization_id,e.started_at,e.ended_at
		FROM events e JOIN visible v ON v.id=e.asset_id WHERE e.status='resolved'
		AND e.started_at>=now()-interval '7 days' AND e.ended_at>=now()-interval '6 hours'
	) rows ORDER BY coalesce(ended_at,started_at) DESC,id LIMIT 24
)
SELECT jsonb_build_object('observedAt',now(),'staleAfter',$3,
	'total',(SELECT count(*) FROM visible),'openEvents',(SELECT count(*) FROM active),
	'fresh',(SELECT count(*) FROM visible WHERE freshness='ok'),
	'assets',coalesce((SELECT jsonb_agg(value) FROM readings),'[]'::jsonb),
	'events',coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'kind',kind,'title',title,
		'status',status,'assetId',asset_id,'assetName',asset_name,'organizationId',organization_id,
		'startedAt',started_at,'endedAt',ended_at) ORDER BY coalesce(ended_at,started_at) DESC,id) FROM recent),'[]'::jsonb))`
