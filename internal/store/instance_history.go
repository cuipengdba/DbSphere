package store

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

type instanceMetric struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Group    string `json:"group"`
	Unit     string `json:"unit"`
	Source   string `json:"source"`
	Path     string `json:"path"`
	Reset    string `json:"reset,omitempty"`
	Mode     string `json:"mode,omitempty"`
	MinMajor int    `json:"minMajor,omitempty"`
}

var instanceMetrics = []instanceMetric{
	{Key: "sessions", Label: "实例客户端连接", Group: "sessions", Unit: "count", Source: "pg_stat_activity", Path: "diagnostics,instanceSessions,data,total"},
	{Key: "connectionUsage", Label: "实例连接占用率", Group: "sessions", Unit: "percent", Source: "pg_stat_activity / max_connections", Path: "diagnostics,instanceSessions,data,usedPercent"},
	{Key: "activeSessions", Label: "活跃客户端会话", Group: "sessions", Unit: "count", Source: "pg_stat_activity", Path: "diagnostics,instanceSessions,data,active"},
	{Key: "waitingSessions", Label: "活跃且有等待事件", Group: "sessions", Unit: "count", Source: "pg_stat_activity", Path: "diagnostics,instanceSessions,data,waiting"},
	{Key: "blocked", Label: "纳管库阻塞会话", Group: "sessions", Unit: "count", Source: "pg_blocking_pids", Path: "diagnostics,locks,data,blockedSessions"},
	{Key: "commits", Label: "提交速率", Group: "transactions", Unit: "rate", Source: "pg_stat_database", Path: "xact_commit", Reset: "diagnostics,database,data,statsReset", Mode: "rate"},
	{Key: "rollbacks", Label: "回滚速率", Group: "transactions", Unit: "rate", Source: "pg_stat_database", Path: "xact_rollback", Reset: "diagnostics,database,data,statsReset", Mode: "rate"},
	{Key: "long", Label: "长事务", Group: "transactions", Unit: "count", Source: "pg_stat_activity", Path: "diagnostics,transactions,data,longTransactions"},
	{Key: "idleTx", Label: "空闲事务", Group: "transactions", Unit: "count", Source: "pg_stat_activity", Path: "diagnostics,transactions,data,idleInTransaction"},
	{Key: "deadlocks", Label: "死锁增量", Group: "transactions", Unit: "count", Source: "pg_stat_database", Path: "diagnostics,database,data,deadlocks", Reset: "diagnostics,database,data,statsReset", Mode: "delta"},
	{Key: "blocksRead", Label: "读入块增量", Group: "io", Unit: "blocks", Source: "pg_stat_database", Path: "diagnostics,database,data,blocksRead", Reset: "diagnostics,database,data,statsReset", Mode: "delta"},
	{Key: "blocksHit", Label: "命中块增量", Group: "io", Unit: "blocks", Source: "pg_stat_database", Path: "diagnostics,database,data,blocksHit", Reset: "diagnostics,database,data,statsReset", Mode: "delta"},
	{Key: "tempFiles", Label: "临时文件增量", Group: "io", Unit: "count", Source: "pg_stat_database", Path: "diagnostics,database,data,tempFiles", Reset: "diagnostics,database,data,statsReset", Mode: "delta"},
	{Key: "tempBytes", Label: "临时写入增量", Group: "io", Unit: "bytes", Source: "pg_stat_database", Path: "diagnostics,database,data,tempBytes", Reset: "diagnostics,database,data,statsReset", Mode: "delta"},
	{Key: "ioReadTime", Label: "读块累计耗时增量", Group: "io", Unit: "ms", Source: "pg_stat_database / track_io_timing", Path: "diagnostics,database,data,readTimeMs", Reset: "diagnostics,database,data,statsReset", Mode: "delta"},
	{Key: "ioWriteTime", Label: "写块累计耗时增量", Group: "io", Unit: "ms", Source: "pg_stat_database / track_io_timing", Path: "diagnostics,database,data,writeTimeMs", Reset: "diagnostics,database,data,statsReset", Mode: "delta"},
	{Key: "ioReads", Label: "实例客户端关系读操作速率", Group: "io", Unit: "rate", Source: "pg_stat_io", Path: "diagnostics,instanceIO,data,reads", Reset: "diagnostics,instanceIO,data,statsReset", Mode: "rate", MinMajor: 16},
	{Key: "ioWrites", Label: "实例客户端关系写操作速率", Group: "io", Unit: "rate", Source: "pg_stat_io", Path: "diagnostics,instanceIO,data,writes", Reset: "diagnostics,instanceIO,data,statsReset", Mode: "rate", MinMajor: 16},
	{Key: "wal", Label: "WAL 产生速率", Group: "wal", Unit: "bytesRate", Source: "pg_stat_wal", Path: "diagnostics,wal,data,bytes", Reset: "diagnostics,wal,data,statsReset", Mode: "rate"},
	{Key: "walPressure", Label: "WAL 缓冲写入增量", Group: "wal", Unit: "count", Source: "pg_stat_wal", Path: "diagnostics,wal,data,buffersFull", Reset: "diagnostics,wal,data,statsReset", Mode: "delta"},
	{Key: "timed", Label: "定时检查点计数增量", Group: "wal", Unit: "count", Source: "checkpoint", Path: "diagnostics,checkpoints,data,timed", Reset: "diagnostics,checkpoints,data,statsReset", Mode: "delta"},
	{Key: "requested", Label: "请求检查点计数增量", Group: "wal", Unit: "count", Source: "checkpoint", Path: "diagnostics,checkpoints,data,requested", Reset: "diagnostics,checkpoints,data,statsReset", Mode: "delta"},
	{Key: "written", Label: "检查点写入缓冲块增量", Group: "wal", Unit: "blocks", Source: "checkpoint", Path: "diagnostics,checkpoints,data,writtenBuffers", Reset: "diagnostics,checkpoints,data,statsReset", Mode: "delta"},
	{Key: "checkpointWrite", Label: "检查点写入耗时增量", Group: "wal", Unit: "ms", Source: "checkpoint", Path: "diagnostics,checkpoints,data,writeMs", Reset: "diagnostics,checkpoints,data,statsReset", Mode: "delta"},
	{Key: "checkpointSync", Label: "检查点同步耗时增量", Group: "wal", Unit: "ms", Source: "checkpoint", Path: "diagnostics,checkpoints,data,syncMs", Reset: "diagnostics,checkpoints,data,statsReset", Mode: "delta"},
}

func (s *Postgres) instanceHistory(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	minutes, bucketErr := strconv.Atoi(params.Get("bucketMinutes"))
	if err != nil || params.Get("to") == "" || bucketErr != nil ||
		(minutes != 1 && minutes != 5 && minutes != 15 && minutes != 60) || to.Sub(from) > time.Duration(400*minutes)*time.Minute {
		return nil, ErrInvalidRange
	}
	rows, err := s.queryJSON(ctx, instanceHistorySQL, id, from, to, minutes, instanceMetrics)
	return map[string]any{"series": rows, "metrics": instanceMetrics}, err
}

const instanceHistorySQL = `WITH observed AS (
	SELECT id,collected_at,metrics_json AS m,metrics_json->>'role' AS role,
		metrics_json#>>'{diagnostics,walPosition,data,startedAt}' AS started,
		metrics_json#>>'{diagnostics,walPosition,data,timeline}' AS timeline,
		(status='ok' OR metrics_json#>>'{collector,pgStatus}'='ok') AS pg_ok
	FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
), samples_with_metrics AS (
	SELECT id,collected_at,pg_ok,d.key,d.mode,started,role,timeline,m#>>string_to_array(d.reset,',') AS reset,
		CASE WHEN pg_ok AND (m#>>string_to_array(d.path,',')) ~ '^[0-9]+([.][0-9]+)?([Ee][+-]?[0-9]+)?$'
			THEN (m#>>string_to_array(d.path,','))::numeric END AS value
	FROM observed CROSS JOIN jsonb_to_recordset($5::jsonb) AS d(key text,mode text,path text,reset text)
), pairs AS (
	SELECT *,lag(value) OVER chronological AS before,lag(reset) OVER chronological AS prior_reset,
		lag(started) OVER chronological AS prior_start,lag(role) OVER chronological AS prior_role,
		lag(timeline) OVER chronological AS prior_timeline,
		extract(epoch FROM collected_at-lag(collected_at) OVER chronological) AS seconds
	FROM samples_with_metrics WINDOW chronological AS (PARTITION BY key ORDER BY collected_at,id)
), values_by_bucket AS (
	SELECT date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz) AS bucket,key,
		CASE mode WHEN 'rate' THEN round(sum(change)/nullif(sum(seconds) FILTER (WHERE change IS NOT NULL),0),2)
			WHEN 'delta' THEN round(sum(change),2) ELSE round(avg(value),2) END AS metric
	FROM (SELECT *,CASE WHEN seconds BETWEEN 1 AND 600 AND value>=before
		AND reset IS NOT DISTINCT FROM prior_reset AND started IS NOT DISTINCT FROM prior_start
		AND role IS NOT DISTINCT FROM prior_role AND timeline IS NOT DISTINCT FROM prior_timeline THEN value-before END AS change FROM pairs) p
	GROUP BY bucket,key,mode
), buckets AS (
	SELECT bucket,jsonb_object_agg(key,metric) AS metric_values FROM values_by_bucket GROUP BY bucket
), coverage AS (
	SELECT date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz) AS bucket,
		count(*)::int AS samples,count(*) FILTER (WHERE pg_ok)::int AS valid FROM observed GROUP BY bucket
)
SELECT jsonb_build_object('bucket',bucket,'values',metric_values,'samples',samples,'observed',valid)
FROM buckets JOIN coverage USING(bucket) ORDER BY bucket`
