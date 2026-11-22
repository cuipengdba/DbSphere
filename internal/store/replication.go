package store

import (
	"context"
	"net/url"
	"time"
)

func (s *Postgres) replicationHistory(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	if err != nil || params.Get("to") == "" {
		return nil, ErrInvalidRange
	}
	at := to
	if params.Get("at") != "" {
		at, err = time.Parse(time.RFC3339Nano, params.Get("at"))
		if err != nil || at.Before(from) || !at.Before(to) {
			return nil, ErrInvalidRange
		}
	}
	topology, err := s.queryJSON(ctx, replicationTopologySQL, id, from, to, at)
	if err != nil || params.Get("view") == "topology" {
		return map[string]any{"topology": topology}, err
	}
	observations, err := s.queryJSON(ctx, replicationObservationsSQL, id, from, to)
	if err != nil {
		return nil, err
	}
	changes, err := s.queryJSON(ctx, replicationChangesSQL, id, from, to)
	return map[string]any{"observations": observations, "changes": changes, "topology": topology}, err
}

const replicationTopologySQL = `WITH metadata AS (
	SELECT id,collected_at,metrics_json,
		CASE WHEN status='ok' OR metrics_json#>>'{collector,pgStatus}'='ok' THEN 'available'
			WHEN metrics_json#>>'{collector,pgStatus}'='availability' THEN 'unavailable' ELSE 'unknown' END AS pg_state
	FROM samples WHERE asset_id=$1 AND collected_at >= $2 AND collected_at < $3
), signatures AS (
	SELECT id,collected_at,pg_state,jsonb_build_object(
		'status',pg_state,'role',metrics_json->'role','endpoint',metrics_json->'postgresEndpoint',
		'timeline',metrics_json#>'{diagnostics,walPosition,data,timeline}',
		'paused',metrics_json#>'{diagnostics,walPosition,data,replayPaused}',
		'receiver',metrics_json->'wal_receiver','upstream',metrics_json#>'{diagnostics,walReceiver,data,senderHost}',
		'senders',metrics_json->'replicas','pool',metrics_json#>'{pgbouncer,route}',
		'senderStates',(SELECT jsonb_agg(jsonb_build_array(link->'application',link->'clientAddress',link->'state',link->'syncState')
			ORDER BY link->>'pid') FROM jsonb_array_elements(coalesce(metrics_json#>'{diagnostics,replicationLinks,data,links}','[]')) link),
		'archiveFailure',metrics_json#>'{diagnostics,archiver,data,lastFailedAt}',
		'poolWaiting',EXISTS(SELECT FROM jsonb_array_elements(coalesce(metrics_json#>'{pgbouncer,pools}','[]')) p
			WHERE jsonb_typeof(p->'clientWaiting')='number' AND (p->>'clientWaiting')::numeric>0),
		'poolStatus',metrics_json#>'{pgbouncer,status}','patroniStatus',metrics_json#>'{patroni,status}',
		'patroniRoles',(SELECT jsonb_object_agg(m->>'name',m->'role')
			FROM jsonb_array_elements(coalesce(metrics_json#>'{patroni,members}','[]')) m WHERE m->>'name' IS NOT NULL),
		'services',metrics_json#>'{k8s,services}',
		'podUid',metrics_json#>'{k8s,pod,uid}',
		'podReady',jsonb_path_query_first(metrics_json,'$.k8s.pod.conditions[*] ? (@.type == "Ready").status')) AS signature
	FROM metadata
), compared AS (
	SELECT *,lag(signature) OVER chronological AS previous,lag(collected_at) OVER chronological AS previous_at
	FROM signatures WINDOW chronological AS (ORDER BY collected_at,id)
), chosen AS (
	(SELECT id,'point' AS kind FROM metadata WHERE collected_at <= $4 ORDER BY collected_at DESC,id DESC LIMIT 1)
	UNION ALL
	(SELECT id,'change' AS kind FROM compared WHERE previous IS DISTINCT FROM signature OR collected_at-previous_at > interval '10 minutes'
		ORDER BY collected_at DESC,id DESC LIMIT 60)
)
SELECT jsonb_build_object('id',m.id,'kind',c.kind,'collectedAt',m.collected_at,'postgresState',m.pg_state,
	'role',m.metrics_json->'role','endpoint',m.metrics_json->'postgresEndpoint',
	'timeline',m.metrics_json#>'{diagnostics,walPosition,data,timeline}',
	'paused',m.metrics_json#>'{diagnostics,walPosition,data,replayPaused}',
	'replayGap',m.metrics_json#>'{diagnostics,walPosition,data,localReplayGapBytes}',
	'receiverState',m.metrics_json->'wal_receiver','receiver',m.metrics_json#>'{diagnostics,walReceiver,data}',
	'senders',m.metrics_json#>'{diagnostics,replicationLinks,data,links}',
	'archive',m.metrics_json#>'{diagnostics,archiver,data}',
	'patroni',m.metrics_json->'patroni','pool',m.metrics_json->'pgbouncer',
	'services',m.metrics_json#>'{k8s,services}','namespace',m.metrics_json#>'{k8s,namespace}',
	'pod',m.metrics_json#>'{k8s,pod,name}',
	'podReady',jsonb_path_query_first(m.metrics_json,'$.k8s.pod.conditions[*] ? (@.type == "Ready").status'))
FROM chosen c JOIN metadata m ON m.id=c.id ORDER BY c.kind DESC,m.collected_at DESC,m.id DESC`

const replicationObservationsSQL = `WITH recent AS (
	SELECT id,collected_at,status,metrics_json FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
	ORDER BY collected_at DESC,id DESC LIMIT 1
), peak AS (
	SELECT id,collected_at,status,metrics_json FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
	AND status='ok' AND jsonb_typeof(metrics_json->'replication_lag_bytes')='number'
	AND (metrics_json->>'replication_lag_bytes')::numeric > 0
	AND metrics_json#>>'{diagnostics,replicationLinks,status}'='ok'
	ORDER BY (metrics_json->>'replication_lag_bytes')::numeric DESC,collected_at,id LIMIT 1
), chosen AS (
	SELECT 'latest' AS kind,* FROM recent
	UNION ALL SELECT 'peak' AS kind,* FROM peak WHERE id NOT IN (SELECT id FROM recent)
)
SELECT jsonb_build_object('id',id,'kind',kind,'collectedAt',collected_at,'status',status,
	'role',metrics_json->'role','receiverState',metrics_json->'wal_receiver',
	'links',metrics_json#>'{diagnostics,replicationLinks}', 'receiver',metrics_json#>'{diagnostics,walReceiver}',
	'position',metrics_json#>'{diagnostics,walPosition}','wal',metrics_json#>'{diagnostics,wal}',
	'archive',metrics_json#>'{diagnostics,archiver}','slots',metrics_json#>'{diagnostics,slotDetails}',
	'freeze',metrics_json#>'{diagnostics,freeze}','patroni',metrics_json->'patroni','pooler',metrics_json->'pgbouncer')
FROM chosen ORDER BY kind`

const replicationChangesSQL = `WITH observations AS (
	SELECT id,collected_at,CASE WHEN status='ok' THEN metrics_json->>'role' END AS role,
		metrics_json#>>'{diagnostics,walPosition,data,timeline}' AS timeline,
		metrics_json#>>'{diagnostics,walPosition,data,startedAt}' AS started,
		metrics_json#>>'{diagnostics,wal,data,statsReset}' AS wal_reset,
		metrics_json#>>'{diagnostics,archiver,data,statsReset}' AS archive_reset
	FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
), compared AS (
	SELECT *,lag(id) OVER chronological AS prior_id,lag(collected_at) OVER chronological AS prior_at,
		lag(role) OVER chronological AS prior_role,lag(timeline) OVER chronological AS prior_timeline,
		lag(started) OVER chronological AS prior_start,lag(wal_reset) OVER chronological AS prior_wal_reset,
		lag(archive_reset) OVER chronological AS prior_archive_reset
	FROM observations WINDOW chronological AS (ORDER BY collected_at,id)
)
SELECT jsonb_build_object('id',id,'collectedAt',collected_at,'priorId',prior_id,'priorAt',prior_at,
	'role',role,'priorRole',prior_role,'timeline',timeline,'priorTimeline',prior_timeline,
	'restarted',started<>prior_start,'walReset',wal_reset<>prior_wal_reset,'archiveReset',archive_reset<>prior_archive_reset,
	'gapSeconds',extract(epoch FROM collected_at-prior_at))
FROM compared WHERE prior_id IS NOT NULL AND (role IS DISTINCT FROM prior_role OR timeline<>prior_timeline
	OR started<>prior_start OR wal_reset<>prior_wal_reset OR archive_reset<>prior_archive_reset)
ORDER BY collected_at DESC,id DESC LIMIT 60`
