package store

import (
	"context"
	"net/url"
	"time"
)

func (s *Postgres) engineDiagnosis(ctx context.Context, assetID string, params url.Values) (any, error) {
	from, to, err := timeRange(params, time.Hour, 24*time.Hour)
	if err != nil {
		return nil, err
	}
	rows, err := s.queryJSON(ctx, engineDiagnosisSQL, assetID, from, to)
	if err != nil {
		return nil, err
	}
	const maxSamples = 100
	truncated := len(rows) > maxSamples
	if truncated {
		rows = rows[:maxSamples]
	}
	return map[string]any{"from": from, "to": to, "samples": rows, "truncated": truncated}, nil
}

// Use actual samples and exact evidence references. An overlapping incident
// alone does not make a sample a trigger or recovery observation.
const engineDiagnosisSQL = `WITH recent AS MATERIALIZED (
	SELECT s.id,s.collected_at,s.status,s.error,s.metrics_json AS m,a.runtime AS engine
	FROM samples s JOIN assets a ON a.id=s.asset_id
	WHERE s.asset_id=$1 AND a.runtime IN ('mysql','redis','etcd') AND s.collected_at>=$2 AND s.collected_at<$3
	ORDER BY s.collected_at DESC,s.id DESC LIMIT 102
), history AS (
	SELECT * FROM recent
	UNION ALL
	SELECT * FROM (
		SELECT s.id,s.collected_at,s.status,s.error,s.metrics_json AS m,a.runtime AS engine
		FROM samples s JOIN assets a ON a.id=s.asset_id
		WHERE s.asset_id=$1 AND a.runtime='redis' AND s.collected_at<$2
		AND s.collected_at>=$2-interval '1 hour'
		ORDER BY s.collected_at DESC,s.id DESC LIMIT 1
	) baseline
), observations AS (
	SELECT history.*,lag(status) OVER reading AS prior_status,
		lag(m->'redis') OVER reading AS prior_redis
	FROM history WINDOW reading AS (ORDER BY collected_at,id)
), event_samples AS MATERIALIZED (
	SELECT ev.payload_json->>'sampleId' AS sample_id,e.id,e.title,e.status,ev.kind
	FROM events e JOIN evidence ev ON ev.event_id=e.id
	WHERE e.asset_id=$1 AND e.started_at<$3 AND (e.ended_at IS NULL OR e.ended_at>=$2)
	AND ev.observed_at>=$2 AND ev.observed_at<$3
	AND e.event_group IN ('availability','telemetry',
		'mysql_lock_wait','mysql_ha_replication','redis_eviction','redis_slots_unhealthy','redis_sentinel_failover',
		'etcd_health_degraded','etcd_disk_latency','etcd_leader_change')
	AND ev.kind IN ('collection_error','collection_recovered','alert_firing','alert_unsuppressed','alert_recovered',
		'mysql_lock_wait','mysql_ha_replication','redis_eviction','redis_slots_unhealthy','redis_sentinel_failover',
		'etcd_health_degraded','etcd_disk_latency','etcd_leader_change')
	AND ev.payload_json->>'sampleId' IN (SELECT id::text FROM recent)
), projected AS (
	SELECT o.*,o.m->o.engine AS data,o.m->(o.engine||'Interval') AS comparison
	FROM observations o WHERE o.collected_at>=$2
)
SELECT jsonb_build_object('sampleId',p.id,'at',p.collected_at,'status',p.status,'error',p.error,
	'role',p.data->>'role','version',p.data->>'version','adapterRole',p.m#>>'{adapter,role}',
	'signals',p.m#>'{adapter,signals}',
	'data',CASE p.engine
		WHEN 'mysql' THEN jsonb_build_object('lockWaits',p.data->'lockWaits','locksTruncated',p.data->'locksTruncated',
			'activeTransactions',p.data->'activeTransactions','topSql',p.data->'topSql','replication',p.data->'replication')
		WHEN 'redis' THEN jsonb_build_object('mode',p.data->'mode','memoryBytes',p.data->'memoryBytes',
			'maxMemoryBytes',p.data->'maxMemoryBytes','latencyMs',p.data->'latencyMs','ops',p.data->'ops',
			'blocked',p.data->'blocked','slotsFail',p.data->'slotsFail')
		WHEN 'etcd' THEN jsonb_build_object('health',p.data->'health','hasLeader',p.data->'hasLeader',
			'leaderId',p.data->'leaderId','memberId',p.data->'memberId','proposalsPending',p.data->'proposalsPending',
			'apiContext',p.data#>>'{kubernetes,context}') END,
	'interval',p.comparison,'redisWindowState',CASE WHEN p.engine='redis' AND p.status='ok' AND p.data->>'mode'<>'sentinel' THEN
		CASE WHEN p.prior_status IS NULL THEN 'baseline_missing'
			WHEN p.prior_status<>'ok' THEN 'sample_gap'
			WHEN NULLIF(p.data->>'runId','') IS NOT NULL AND NULLIF(p.prior_redis->>'runId','') IS NOT NULL
				AND p.data->>'runId'<>p.prior_redis->>'runId' THEN 'process_changed'
			WHEN EXISTS (SELECT 1 FROM unnest(ARRAY['evicted','hits','misses','rejected']) AS fields(key)
				WHERE p.data->>'runId'=p.prior_redis->>'runId' AND NULLIF(p.data->>'runId','') IS NOT NULL
				AND p.data->>'mode'=p.prior_redis->>'mode'
				AND jsonb_typeof(p.data->fields.key)='number' AND jsonb_typeof(p.prior_redis->fields.key)='number'
				AND (p.data->fields.key)<(p.prior_redis->fields.key)) THEN 'counter_reset'
			WHEN p.comparison IS NULL THEN 'window_missing'
			ELSE 'observed' END END,
	'events',coalesce((SELECT jsonb_agg(link) FROM (
		SELECT DISTINCT jsonb_build_object('id',e.id,'title',e.title,'status',e.status,
			'phase',CASE WHEN e.kind IN ('collection_recovered','alert_recovered') THEN 'recovery' ELSE 'trigger' END) AS link
		FROM event_samples e WHERE e.sample_id=p.id::text ORDER BY link LIMIT 8
	) unique_links),'[]'::jsonb))
FROM projected p ORDER BY p.collected_at DESC,p.id DESC LIMIT 101`
