package store

import (
	"context"
	"encoding/json"
	"net/url"
	"time"
)

func (s *Postgres) containerStorageHistory(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, time.Hour, 366*24*time.Hour)
	name, device := params.Get("container"), params.Get("device")
	if err != nil || len(name) > 63 || len(device) > 128 {
		return nil, ErrInvalidRange
	}
	step := max(10, int(to.Sub(from).Seconds()/240)+1)
	rows, err := s.queryJSON(ctx, containerHistorySQL, id, from, to, name, device, step)
	if err != nil {
		return nil, err
	}
	latest, err := s.queryJSON(ctx, `SELECT jsonb_build_object('sampleId',s.id,'at',s.collected_at,
		'storage',s.metrics_json->'containerStorage','nodeRates',s.metrics_json#>'{linux,deviceRates,disk}',
		'nodeNumbers',s.metrics_json#>'{linux,deviceNumbers}',
		'pgIO',s.metrics_json->'pgStorageIO','pgSQL',s.metrics_json#>'{sqlPerformance,statements}')
		FROM samples s WHERE s.asset_id=$1 AND s.collected_at >= $2 AND s.collected_at <= $3
		AND s.metrics_json ? 'containerStorage' ORDER BY s.collected_at DESC LIMIT 1`, id, from, to)
	if err != nil {
		return nil, err
	}
	events, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id', id, 'title', title, 'status', status,
		'at', started_at, 'endedAt', ended_at) FROM events WHERE asset_id=$1 AND started_at <= $3
		AND coalesce(ended_at,$3) >= $2 AND event_group IN ('storage','latency','availability')
		ORDER BY started_at DESC LIMIT 80`, id, from, to)
	if err != nil {
		return nil, err
	}
	var recent json.RawMessage
	if len(latest) > 0 {
		recent = latest[0]
	}
	return map[string]any{"assetId": id, "from": from, "to": to, "stepSeconds": step,
		"buckets": rows, "latest": recent, "events": events}, nil
}

const containerHistorySQL = `WITH observations AS (
	SELECT date_bin(make_interval(secs=>$6::integer),collected_at,$2::timestamptz) AS bucket,
		metrics_json#>'{containerStorage}' AS storage, metrics_json AS metrics
	FROM samples WHERE asset_id=$1 AND collected_at >= $2 AND collected_at <= $3 AND metrics_json ? 'containerStorage'
), selected AS (
	SELECT bucket, storage, metrics, storage->'containers'->coalesce(nullif($4,''), storage->>'primary') AS c
	FROM observations
), points AS (
	SELECT bucket, c, storage, metrics, CASE WHEN $5='' THEN c->'rates' ELSE c->'deviceRates'->$5 END AS rate
	FROM selected
), measures AS (
	SELECT bucket, m.name, min((m.v#>>'{}')::float8) AS low, max((m.v#>>'{}')::float8) AS high,
		avg((m.v#>>'{}')::float8) AS mean, count(*) AS n
	FROM points CROSS JOIN LATERAL (VALUES
		('readBps', rate->'readBps'), ('writeBps', rate->'writeBps'), ('readIops', rate->'readIops'), ('writeIops', rate->'writeIops'),
		('active', c#>'{memory,active_file}'), ('inactive', c#>'{memory,inactive_file}'), ('file',c#>'{memory,file}'),
		('dirty', c#>'{memory,file_dirty}'), ('writeback', c#>'{memory,file_writeback}'),
		('refault', c#>'{rates,refaultPerSecond}'), ('reclaims', c#>'{rates,reclaimsPerSecond}'),
		('ioPressure', c#>'{pressure,io_some_avg10}'), ('memoryPressure',c#>'{pressure,memory_some_avg10}'),
		('pgIOWait', storage->'pgIOWait'), ('pgReadTime',metrics#>'{diagnostics,database,data,readTimeMs}'),
		('tempBytes',metrics#>'{diagnostics,database,data,tempBytes}'), ('checkpointSync',metrics#>'{diagnostics,checkpoints,data,syncMs}')
	) m(name,v) WHERE (c->>'status'='ok' OR m.name IN ('pgIOWait','pgReadTime','tempBytes','checkpointSync')) AND jsonb_typeof(m.v)='number'
	GROUP BY bucket, m.name
), bucket_series AS (
	SELECT bucket,jsonb_object_agg(name,jsonb_build_object('min',low,'max',high,'mean',mean,'count',n)) series
	FROM measures GROUP BY bucket
), coverage AS (
	SELECT bucket, count(*) AS samples, count(*) FILTER (WHERE c->>'status'='ok') AS available,
		bool_or(c->'identityChanged'='true'::jsonb) AS changed,
		bool_or(c->'counterReset'='true'::jsonb) AS reset
	FROM points GROUP BY bucket
)
SELECT jsonb_build_object('at',c.bucket,'samples',c.samples,'available',c.available,'identityChanged',c.changed,'counterReset',c.reset,
	'series',coalesce(b.series,'{}'::jsonb)) FROM coverage c LEFT JOIN bucket_series b USING(bucket) ORDER BY c.bucket`
