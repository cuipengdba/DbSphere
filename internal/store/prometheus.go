package store

import (
	"context"
	"encoding/json"
)

type PrometheusAsset struct {
	ID, Name, Organization, Cluster, Engine, Runtime                      string
	SampleAt, Fresh, OpenEvents                                           *float64
	Connections, MaxConnections, Blocked                                  *float64
	ReplicationLag, Transactions, LinuxCPU, LinuxRAM                      *float64
	PodReady, PGWaiting, PGSQLFingerprints                                *float64
	LinuxRX, LinuxTX, LinuxDiskRead, LinuxDiskWrite                       *float64
	PodCPU, PodMemory                                                     *float64
	MySQLRunning, MySQLLockWaits                                          *float64
	DM8Sessions, DM8Active, DM8Transactions, DM8Waits                     *float64
	DM8ReplayGap, DM8Switches                                             *float64
	KingbaseSessions, KingbaseActive, KingbaseTransactions, KingbaseWaits *float64
	KingbaseSize, KingbaseLag                                             *float64
	KingbaseStandbyLag, KingbaseArchiveFailures                           *float64
	RedisOps, RedisClients, RedisMemory, RedisHit                         *float64
	RedisEvicted, RedisRejected, RedisLatency, RedisSlots                 *float64
	EtcdHasLeader, EtcdElections, EtcdPending, EtcdSize, EtcdQuota        *float64
	EtcdWAL, EtcdCommit, EtcdAPILatency                                   *float64
	Container                                                             json.RawMessage
}

func (s *Postgres) PrometheusAssets(ctx context.Context, staleAfter int) ([]PrometheusAsset, error) {
	rows, err := s.pool.Query(ctx, prometheusAssetsSQL, staleAfter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	samples := make([]PrometheusAsset, 0, 64)
	for rows.Next() {
		var a PrometheusAsset
		if err := rows.Scan(&a.ID, &a.Name, &a.Organization, &a.Cluster, &a.Engine, &a.Runtime,
			&a.SampleAt, &a.Fresh, &a.OpenEvents, &a.Connections, &a.MaxConnections, &a.Blocked,
			&a.ReplicationLag, &a.Transactions, &a.LinuxCPU, &a.LinuxRAM, &a.PodReady,
			&a.PGWaiting, &a.PGSQLFingerprints, &a.MySQLRunning, &a.MySQLLockWaits,
			&a.DM8Sessions, &a.DM8Active, &a.DM8Transactions, &a.DM8Waits,
			&a.DM8ReplayGap, &a.DM8Switches,
			&a.KingbaseSessions, &a.KingbaseActive, &a.KingbaseTransactions, &a.KingbaseWaits, &a.KingbaseSize, &a.KingbaseLag,
			&a.KingbaseStandbyLag, &a.KingbaseArchiveFailures,
			&a.RedisOps, &a.RedisClients, &a.RedisMemory, &a.RedisHit, &a.RedisEvicted, &a.RedisRejected, &a.RedisLatency, &a.RedisSlots,
			&a.EtcdHasLeader, &a.EtcdElections, &a.EtcdPending, &a.EtcdSize, &a.EtcdQuota,
			&a.EtcdWAL, &a.EtcdCommit, &a.EtcdAPILatency,
			&a.LinuxRX, &a.LinuxTX, &a.LinuxDiskRead, &a.LinuxDiskWrite, &a.PodCPU, &a.PodMemory, &a.Container); err != nil {
			return nil, err
		}
		samples = append(samples, a)
	}
	return samples, rows.Err()
}

const prometheusAssetsSQL = `WITH selected AS (
	SELECT a.*,last_sample_at IS NOT NULL AND ($1=0 OR last_sample_at>now()-make_interval(secs=>$1)) AS recent
	FROM assets a ORDER BY a.id LIMIT 5000
)
SELECT a.id::text,a.name,a.organization_id::text,coalesce(a.cluster_name,''),
	CASE WHEN a.runtime IN ('mysql','dm8','kingbase','redis','etcd') THEN a.runtime ELSE 'postgres' END,a.runtime,
	extract(epoch FROM a.last_sample_at)::float8,
	CASE WHEN a.recent AND a.last_status='ok' THEN 1.0 ELSE 0.0 END::float8,
	e.total::float8,
	t.connections::float8,t.max_connections::float8,t.blocked_sessions::float8,
	t.replication_lag_bytes::float8,(t.commits + t.rollbacks)::float8,t.linux_cpu::float8,t.linux_memory::float8,
	CASE WHEN raw.metrics_json#>>'{k8s,status}'='ok' THEN (
		SELECT CASE condition->>'status' WHEN 'True' THEN 1.0 WHEN 'False' THEN 0.0 END::float8
		FROM jsonb_array_elements(CASE WHEN jsonb_typeof(raw.metrics_json#>'{k8s,pod,conditions}')='array'
			THEN raw.metrics_json#>'{k8s,pod,conditions}' ELSE '[]'::jsonb END) condition
		WHERE condition->>'type'='Ready' LIMIT 1
	) END,
	CASE WHEN raw.metrics_json#>>'{diagnostics,instanceSessions,status}'='ok'
		AND jsonb_typeof(raw.metrics_json#>'{diagnostics,instanceSessions,data,waiting}')='number'
		THEN (raw.metrics_json#>>'{diagnostics,instanceSessions,data,waiting}')::float8 END,
	CASE WHEN raw.metrics_json#>>'{sqlPerformance,status}'='ok'
		AND jsonb_typeof(raw.metrics_json#>'{sqlPerformance,statements}')='array'
		THEN jsonb_array_length(raw.metrics_json#>'{sqlPerformance,statements}')::float8 END,
	CASE WHEN jsonb_typeof(raw.metrics_json#>'{mysql,running}')='number'
		THEN (raw.metrics_json#>>'{mysql,running}')::float8 END,
	CASE WHEN raw.metrics_json#>'{adapter,signals}' @> '[{"key":"lockWaits","status":"ok"}]'::jsonb
		AND jsonb_typeof(raw.metrics_json#>'{mysql,lockWaits}')='array'
		THEN jsonb_array_length(raw.metrics_json#>'{mysql,lockWaits}')::float8 END,
	(raw.metrics_json#>>'{dm8,sessions}')::float8,
	(raw.metrics_json#>>'{dm8,activeSessions}')::float8,
	(raw.metrics_json#>>'{dm8,transactions}')::float8,
	CASE WHEN raw.metrics_json#>'{adapter,signals}' @> '[{"key":"lockWaits","status":"ok"}]'::jsonb
		AND jsonb_typeof(raw.metrics_json#>'{dm8,waits}')='array'
		THEN jsonb_array_length(raw.metrics_json#>'{dm8,waits}')::float8 END,
	(raw.metrics_json#>>'{dm8,ha,replayGap}')::float8,(raw.metrics_json#>>'{dm8,ha,switches}')::float8,
	(raw.metrics_json#>>'{kingbase,sessions}')::float8,
	(raw.metrics_json#>>'{kingbase,activeSessions}')::float8,
	(raw.metrics_json#>>'{kingbase,transactions}')::float8,
	CASE WHEN raw.metrics_json#>'{adapter,signals}' @> '[{"key":"lockWaits","status":"ok"}]'::jsonb
		AND jsonb_typeof(raw.metrics_json#>'{kingbase,waits}')='array'
		THEN jsonb_array_length(raw.metrics_json#>'{kingbase,waits}')::float8 END,
	(raw.metrics_json#>>'{kingbase,databaseBytes}')::float8,
	(raw.metrics_json#>>'{kingbase,replication,maxReplayLagBytes}')::float8,
	(raw.metrics_json#>>'{kingbase,ha,replayGap}')::float8,(raw.metrics_json#>>'{kingbase,ha,archive,failed}')::float8,
	(raw.metrics_json#>>'{redis,ops}')::float8,
	(raw.metrics_json#>>'{redis,clients}')::float8,
	(raw.metrics_json#>>'{redis,memoryBytes}')::float8,
	(raw.metrics_json#>>'{redisInterval,hitPercent}')::float8,
	(raw.metrics_json#>>'{redisInterval,evicted}')::float8,
	(raw.metrics_json#>>'{redisInterval,rejected}')::float8,
	(raw.metrics_json#>>'{redis,latencyMs}')::float8,
	(raw.metrics_json#>>'{redis,slotsOk}')::float8,
	(raw.metrics_json#>>'{etcd,hasLeader}')::float8,
	(raw.metrics_json#>>'{etcd,leaderChanges}')::float8,
	(raw.metrics_json#>>'{etcd,proposalsPending}')::float8,
	(raw.metrics_json#>>'{etcd,sizeBytes}')::float8,
	(raw.metrics_json#>>'{etcd,quotaBytes}')::float8,
	(raw.metrics_json#>>'{etcdInterval,wal,meanMs}')::float8,
	(raw.metrics_json#>>'{etcdInterval,backend,meanMs}')::float8,
	(raw.metrics_json#>>'{etcdInterval,kubernetes,meanMs}')::float8,
	(raw.metrics_json#>>'{linux,interval,networkReceiveBps}')::float8,
	(raw.metrics_json#>>'{linux,interval,networkTransmitBps}')::float8,
	(raw.metrics_json#>>'{linux,interval,diskReadBps}')::float8,
	(raw.metrics_json#>>'{linux,interval,diskWriteBps}')::float8,
	(raw.metrics_json#>>'{k8s,resourceMetrics,podUsage,cpuCores}')::float8,
	(raw.metrics_json#>>'{k8s,resourceMetrics,podUsage,memoryBytes}')::float8,
	CASE WHEN raw.metrics_json ? 'containerStorage' THEN jsonb_build_object(
		'storage',raw.metrics_json->'containerStorage','node',raw.metrics_json#>'{linux,deviceRates,disk}',
		'nodeNumbers',raw.metrics_json#>'{linux,deviceNumbers}',
		'pgDatabase',raw.metrics_json#>'{diagnostics,database,data}',
		'checkpoints',raw.metrics_json#>'{diagnostics,checkpoints,data}', 'wal',raw.metrics_json#>'{diagnostics,wal,data}') END
FROM selected a
LEFT JOIN LATERAL (SELECT count(*) AS total FROM open_event_groups WHERE asset_id=a.id) e ON true
LEFT JOIN LATERAL (SELECT connections,max_connections,blocked_sessions,replication_lag_bytes,commits,rollbacks,linux_cpu,linux_memory
	FROM monitoring_trend_points WHERE asset_id=a.id AND collected_at=a.last_sample_at ORDER BY id DESC LIMIT 1) t ON a.recent
LEFT JOIN LATERAL (SELECT metrics_json FROM samples WHERE asset_id=a.id AND collected_at=a.last_sample_at
	ORDER BY id DESC LIMIT 1) raw ON a.recent
ORDER BY a.id`

// NodeExporterDiscovery publishes only targets explicitly configured on a Linux asset.
func (s *Postgres) NodeExporterDiscovery(ctx context.Context) (json.RawMessage, error) {
	var names json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg(jsonb_build_object(
		'targets',jsonb_build_array((CASE WHEN strpos(linux_host,':')>0 THEN '['||linux_host||']' ELSE linux_host END)||':'||linux_exporter_port::text),
		'labels',jsonb_build_object('asset_id',id,'asset_name',name,'organization_id',organization_id,
			'cluster',coalesce(cluster_name,''),'engine','postgres','runtime','linux')) ORDER BY id),'[]'::jsonb)
		FROM (SELECT id,name,organization_id,cluster_name,linux_host,linux_exporter_port FROM assets
			WHERE runtime='linux' AND nullif(linux_host,'') IS NOT NULL AND linux_exporter_port>0
			ORDER BY id LIMIT 5000) targets`).Scan(&names)
	return names, err
}
