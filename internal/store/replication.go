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
	observations, err := s.queryJSON(ctx, replicationObservationsSQL, id, from, to)
	if err != nil {
		return nil, err
	}
	changes, err := s.queryJSON(ctx, replicationChangesSQL, id, from, to)
	return map[string]any{"observations": observations, "changes": changes}, err
}

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
