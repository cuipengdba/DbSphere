package store

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"
)

type LiveScope struct {
	Organization, Environment, Cluster, Engine string
	Wallboard                                  bool
	UnassignedHosts                            bool
	Page                                       int
	Focus                                      string
	BoardState                                 string
}

func IsLiveEngine(engine string) bool {
	switch engine {
	case "postgres", "mysql", "dm8", "kingbase", "opengauss", "redis", "etcd":
		return true
	default:
		return false
	}
}

// LiveObservation applies the authorized scope before ranking or limiting assets.
func (s *Postgres) LiveObservation(ctx context.Context, ids []string, staleAfter int, scope LiveScope) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var result json.RawMessage
	err := s.pool.QueryRow(ctx, liveObservationSQL, ids, OrganizationFromContext(ctx), staleAfter,
		scope.Organization, scope.Environment, scope.Cluster, scope.Engine, scope.Wallboard,
		scope.Page, scope.Focus, scope.BoardState).Scan(&result)
	if err == nil && scope.Wallboard && scope.UnassignedHosts && ids == nil && scope.Organization == "" &&
		scope.Environment == "" && scope.Cluster == "" && scope.Engine == "" && scope.BoardState == "" && scope.Page == 0 {
		return s.addUnassignedWallHosts(result)
	}
	return result, err
}

// Collector-only nodes have no tenant assignment. Only a platform administrator
// viewing the unfiltered board can see these configured sources.
func (s *Postgres) addUnassignedWallHosts(snapshot json.RawMessage) (json.RawMessage, error) {
	var result map[string]json.RawMessage
	if err := json.Unmarshal(snapshot, &result); err != nil {
		return nil, err
	}
	var assigned []string
	if err := json.Unmarshal(result["assignedNodes"], &assigned); err != nil {
		return nil, err
	}
	unassigned := []map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(s.containerStorage.Nodes)) {
		if !slices.Contains(assigned, name) {
			unassigned = append(unassigned, map[string]string{"name": name, "source": "采集源"})
		}
	}
	result["unassignedHosts"], _ = json.Marshal(unassigned)
	return json.Marshal(result)
}

const liveObservationSQL = `WITH visible AS MATERIALIZED (
	SELECT a.id,a.name,a.cluster_name,a.runtime,a.environment,a.organization_id,o.name AS organization_name,
		CASE WHEN a.runtime IN ('mysql','dm8','kingbase','opengauss','redis','etcd') THEN a.runtime ELSE 'postgres' END AS engine,
		a.role,a.k8s_cluster,a.k8s_namespace,a.k8s_pod,a.k8s_node,a.last_sample_at,a.last_status,a.linux_host,a.linux_exporter_port,
		CASE WHEN a.last_sample_at IS NULL THEN 'pending'
			WHEN $3::int>0 AND a.last_sample_at<=now()-make_interval(secs=>$3) THEN 'stale'
			ELSE a.last_status END AS freshness
	FROM assets a JOIN organizations o ON o.id=a.organization_id
	WHERE ($1::uuid[] IS NULL OR a.id=ANY($1)) AND (nullif($2,'') IS NULL OR a.organization_id=nullif($2,'')::uuid)
), inventory AS MATERIALIZED (
	SELECT *,CASE WHEN cluster_name IS NULL OR cluster_name='' THEN id::text
		ELSE organization_id::text || '/' || engine || '/' || cluster_name END AS cluster_key FROM visible
), filtered_inventory AS MATERIALIZED (
	SELECT * FROM inventory WHERE ($4='' OR organization_id::text=$4) AND ($5='' OR environment=$5)
		AND ($6='' OR cluster_key=$6)
), selected AS MATERIALIZED (
	SELECT * FROM filtered_inventory WHERE $7='' OR engine=$7
), engine_counts AS (
	SELECT i.engine,count(*) AS total,
		count(*) FILTER (WHERE i.freshness='ok') AS fresh,
		count(*) FILTER (WHERE i.freshness NOT IN ('ok','error')) AS missing,
		count(*) FILTER (WHERE i.freshness='error') AS failed,
		count(*) FILTER (WHERE EXISTS(SELECT 1 FROM open_event_groups g WHERE g.asset_id=i.id AND g.event_group='availability')) AS unavailable,
		count(*) FILTER (WHERE i.freshness='error' OR EXISTS(SELECT 1 FROM open_event_groups g WHERE g.asset_id=i.id)) AS affected
	FROM filtered_inventory i WHERE $8 GROUP BY i.engine
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
), recoveries AS MATERIALIZED (
	SELECT e.id,e.kind,e.event_group,e.title,e.status,e.asset_id,v.name AS asset_name,v.organization_id,e.started_at,e.ended_at,NULL::text AS severity
	FROM events e JOIN selected v ON v.id=e.asset_id WHERE e.status='resolved'
	AND e.started_at>=now()-interval '7 days' AND e.ended_at>=now()-interval '6 hours'
), board_assets AS MATERIALIZED (
	SELECT v.*,coalesce(c.total,0) AS open_events,coalesce(c.unavailable,false) AS unavailable,
		r.ended_at AS recovered_at,w.ends_at AS maintenance_until
	FROM selected v LEFT JOIN counts c ON c.asset_id=v.id
	LEFT JOIN (SELECT asset_id,max(ended_at) AS ended_at FROM recoveries WHERE $8 GROUP BY asset_id) r ON r.asset_id=v.id
	LEFT JOIN LATERAL (SELECT max(ends_at) AS ends_at FROM silences WHERE $8 AND kind='maintenance' AND revoked_at IS NULL
		AND starts_at<=now() AND ends_at>now() AND (asset_id=v.id OR asset_id IS NULL)) w ON true
), filtered AS MATERIALIZED (
	SELECT * FROM board_assets WHERE NOT $8 OR CASE $11
		WHEN 'affected' THEN open_events>0 OR freshness='error'
		WHEN 'missing' THEN freshness NOT IN ('ok','error')
		WHEN 'maintenance' THEN maintenance_until IS NOT NULL
		WHEN 'recovered' THEN recovered_at IS NOT NULL
		WHEN 'healthy' THEN open_events=0 AND freshness='ok' AND maintenance_until IS NULL
		ELSE true END
), wall_hosts AS MATERIALIZED (
	SELECT v.id,h.name,h.kind,h.assigned_node,
		CASE WHEN h.identity IS NOT NULL THEN md5(concat_ws(chr(31),v.organization_id::text,v.environment,h.identity)) END AS host_key
	FROM selected v
	LEFT JOIN LATERAL (SELECT metrics_json AS sample FROM samples
		WHERE asset_id=v.id AND collected_at=v.last_sample_at ORDER BY id DESC LIMIT 1) m ON true
	LEFT JOIN LATERAL (SELECT
		CASE WHEN m.sample#>>'{k8s,status}'='ok' AND nullif(m.sample#>>'{k8s,node,uid}','') IS NOT NULL
				AND nullif(v.k8s_cluster,'') IS NOT NULL THEN 'node/' || v.k8s_cluster || '/' || (m.sample#>>'{k8s,node,uid}')
			WHEN v.runtime IN ('linux','dm8','kingbase','opengauss','mysql') AND nullif(v.linux_host,'') IS NOT NULL AND v.linux_exporter_port>0
				THEN 'linux/' || lower(v.linux_host) || '/' || v.linux_exporter_port
			WHEN m.sample#>>'{containerStorage,runtime}'='docker' AND nullif(m.sample#>>'{containerStorage,node}','') IS NOT NULL
				THEN 'docker/' || (m.sample#>>'{containerStorage,node}') END AS identity,
		CASE WHEN nullif(m.sample#>>'{k8s,node,uid}','') IS NOT NULL THEN m.sample#>>'{k8s,node,name}'
			WHEN v.runtime IN ('linux','dm8','kingbase','opengauss','mysql') AND v.linux_exporter_port>0 THEN v.linux_host
			WHEN m.sample#>>'{containerStorage,runtime}'='docker' THEN m.sample#>>'{containerStorage,node}' END AS name,
		CASE WHEN nullif(m.sample#>>'{k8s,node,uid}','') IS NOT NULL THEN 'Kubernetes'
			WHEN m.sample#>>'{containerStorage,runtime}'='docker' THEN 'Docker' ELSE 'Linux' END AS kind,
		CASE WHEN nullif(m.sample#>>'{k8s,node,uid}','') IS NOT NULL
			THEN v.k8s_cluster || '/' || (m.sample#>>'{k8s,node,name}')
			WHEN m.sample#>>'{containerStorage,runtime}'='docker' THEN m.sample#>>'{containerStorage,node}' END AS assigned_node
	) h ON true
	WHERE $8::boolean
), shown AS (
	SELECT v.* FROM filtered v
	LEFT JOIN wall_hosts h ON h.id=v.id
	ORDER BY ($8 AND $9=0 AND v.id::text=$10) DESC,v.unavailable DESC,(v.open_events>0) DESC,
		(v.freshness='error') DESC,CASE WHEN $8 THEN h.host_key END NULLS LAST,v.last_sample_at DESC NULLS LAST,v.id
	LIMIT CASE WHEN $8 THEN 24 ELSE 120 END OFFSET CASE WHEN $8 THEN $9::int*24 ELSE 0 END
), readings AS (
	SELECT a.id,a.unavailable,a.open_events,a.freshness,h.host_key,a.last_sample_at,
		jsonb_build_object('id',a.id,'name',a.name,'cluster',a.cluster_name,'runtime',a.runtime,
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
		'engineMetrics',CASE WHEN a.engine='etcd' THEN jsonb_build_object(
				'health',m.metrics_json#>'{etcd,health}','leaderId',m.metrics_json#>'{etcd,leaderId}',
				'pending',m.metrics_json#>'{etcd,proposalsPending}','size',m.metrics_json#>'{etcd,sizeBytes}',
				'wal',m.metrics_json#>'{etcdInterval,wal,meanMs}','commit',m.metrics_json#>'{etcdInterval,backend,meanMs}',
				'api',m.metrics_json#>'{etcdInterval,kubernetes,meanMs}')
				WHEN a.engine='redis' THEN jsonb_build_object(
				'ops',m.metrics_json#>'{redis,ops}','clients',m.metrics_json#>'{redis,clients}',
				'memory',m.metrics_json#>'{redis,memoryBytes}','hitPercent',m.metrics_json#>'{redisInterval,hitPercent}',
				'evicted',m.metrics_json#>'{redisInterval,evicted}','latency',m.metrics_json#>'{redis,latencyMs}',
				'mode',m.metrics_json#>'{redis,mode}','slots',m.metrics_json#>'{redis,slotsOk}',
				'sentinel',m.metrics_json#>'{redis,sentinel}')
				WHEN a.engine IN ('kingbase','opengauss') THEN jsonb_build_object(
				'sessions',m.metrics_json->a.engine->'sessions','active',m.metrics_json->a.engine->'activeSessions',
				'transactions',m.metrics_json->a.engine->'transactions','size',m.metrics_json->a.engine->'databaseBytes',
				'waits',CASE WHEN m.metrics_json#>'{adapter,signals}' @> '[{"key":"lockWaits","status":"ok"}]'::jsonb
					AND jsonb_typeof(m.metrics_json->a.engine->'waits')='array' THEN jsonb_array_length(m.metrics_json->a.engine->'waits') END)
				END,
		'linux',m.metrics_json#>'{linux,status}','k8s',m.metrics_json#>'{k8s,status}')
		|| CASE WHEN $8 THEN jsonb_build_object('clusterKey',a.cluster_key,'recoveredAt',a.recovered_at,'maintenanceUntil',a.maintenance_until,
			'wallHost',CASE WHEN h.host_key IS NOT NULL THEN
			jsonb_build_object('id',h.host_key,'name',h.name,'kind',h.kind,'cpu',m.metrics_json#>'{linux,cpu,usedPercent}',
				'memory',m.metrics_json#>'{linux,memory,usedPercent}','diskRead',m.metrics_json#>'{linux,interval,diskReadBps}',
				'diskWrite',m.metrics_json#>'{linux,interval,diskWriteBps}','source',m.metrics_json#>>'{linux,source}',
				'status',m.metrics_json#>>'{linux,status}','observedAt',m.metrics_json#>>'{linux,observedAt}') END,
			'container',m.metrics_json#>>'{containerStorage,primary}','containerRuntime',m.metrics_json#>>'{containerStorage,runtime}')
		ELSE '{}'::jsonb END AS value
	FROM shown a LEFT JOIN LATERAL (SELECT id,metrics_json FROM samples
		WHERE asset_id=a.id AND collected_at=a.last_sample_at ORDER BY id DESC LIMIT 1) m ON true
	LEFT JOIN wall_hosts h ON h.id=a.id
), recent AS (
	SELECT *,CASE WHEN severity='critical' THEN 0 WHEN event_group='availability' THEN 1 WHEN severity='warning' THEN 2 ELSE 3 END AS priority FROM (
		SELECT * FROM active
		UNION ALL
		SELECT * FROM recoveries
	) rows ORDER BY (status='open') DESC,priority,coalesce(ended_at,started_at) DESC,id LIMIT 24
)
SELECT jsonb_build_object('observedAt',now(),'staleAfter',$3,
	'total',(SELECT count(*) FROM selected),'openEvents',(SELECT count(*) FROM active),
	'fresh',(SELECT count(*) FROM selected WHERE freshness='ok'),
	'unavailable',(SELECT count(*) FROM counts WHERE unavailable),
	'filters',coalesce((SELECT jsonb_agg(to_jsonb(options)) FROM (SELECT DISTINCT organization_id AS "organizationId",
		organization_name AS organization,environment,engine,cluster_key AS "clusterKey",coalesce(nullif(cluster_name,''),name) AS cluster
		FROM inventory) options),'[]'::jsonb),
	'assets',coalesce((SELECT jsonb_agg(value ORDER BY ($8 AND $9=0 AND id::text=$10) DESC,unavailable DESC,(open_events>0) DESC,
		(freshness='error') DESC,host_key NULLS LAST,last_sample_at DESC NULLS LAST,id) FROM readings),'[]'::jsonb),
	'events',coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'kind',kind,'title',title,
		'status',status,'assetId',asset_id,'assetName',asset_name,'organizationId',organization_id,'severity',severity,'group',event_group,
		'startedAt',started_at,'endedAt',ended_at) ORDER BY (status='open') DESC,priority,coalesce(ended_at,started_at) DESC,id) FROM recent),'[]'::jsonb))
	|| CASE WHEN $8 THEN jsonb_build_object('hostCount',(SELECT count(DISTINCT host_key) FROM wall_hosts),
		'engines',coalesce((SELECT jsonb_agg(to_jsonb(engine_counts) ORDER BY engine) FROM engine_counts),'[]'::jsonb),
		'failedCount',(SELECT count(*) FROM selected WHERE freshness='error'),
		'filteredTotal',(SELECT count(*) FROM filtered),
		'affectedCount',(SELECT count(*) FROM board_assets WHERE open_events>0 OR freshness='error'),
		'missingCount',(SELECT count(*) FROM board_assets WHERE freshness NOT IN ('ok','error')),
		'maintenanceCount',(SELECT count(*) FROM board_assets WHERE maintenance_until IS NOT NULL),
		'recoveredCount',(SELECT count(*) FROM recoveries),
		'assignedNodes',coalesce((SELECT jsonb_agg(DISTINCT assigned_node) FROM wall_hosts WHERE assigned_node IS NOT NULL),'[]'::jsonb))
	ELSE '{}'::jsonb END`
