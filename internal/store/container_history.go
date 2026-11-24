package store

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"time"
)

func (s *Postgres) containerStorageHistory(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, time.Hour, 366*24*time.Hour)
	name, device, pvcUID := params.Get("container"), params.Get("device"), params.Get("pvcUid")
	if err != nil || len(name) > 63 || len(device) > 128 || len(pvcUID) > 128 {
		return nil, ErrInvalidRange
	}
	step := max(10, int(to.Sub(from).Seconds()/240)+1)
	origin := from
	if raw := params.Get("bucketMinutes"); raw != "" {
		minutes, err := strconv.Atoi(raw)
		if err != nil || (minutes != 1 && minutes != 5 && minutes != 15 && minutes != 60) || to.Sub(from) > monitoringBucketLimit(minutes) {
			return nil, ErrInvalidRange
		}
		step = minutes * 60
		origin = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	rows, err := s.queryJSON(ctx, containerHistorySQL, id, from, to, name, device, step, pvcUID, origin)
	if err != nil {
		return nil, err
	}
	latest, err := s.queryJSON(ctx, `SELECT jsonb_build_object('sampleId',s.id,'at',s.collected_at,
		'storage',s.metrics_json->'containerStorage','nodeRates',s.metrics_json#>'{linux,deviceRates,disk}',
		'nodeNumbers',s.metrics_json#>'{linux,deviceNumbers}',
		'host',jsonb_build_object('status',s.metrics_json#>'{linux,status}','cpu',s.metrics_json#>'{linux,cpu,usedPercent}',
			'memory',s.metrics_json#>'{linux,memory,usedPercent}','load',s.metrics_json#>'{linux,cpu,load1}',
			'network',s.metrics_json#>'{linux,interval}','boot',s.metrics_json#>'{linux,bootTimeSeconds}'),
		'kubernetes',jsonb_build_object('status',s.metrics_json#>'{k8s,status}','podUsage',s.metrics_json#>'{k8s,resourceMetrics,podUsage}',
			'phase',s.metrics_json#>'{k8s,pod,phase}','owner',s.metrics_json#>'{k8s,owner}'),
		'pgIO',s.metrics_json->'pgStorageIO','pgSQL',s.metrics_json#>'{sqlPerformance,statements}')
		FROM samples s WHERE s.asset_id=$1 AND s.collected_at >= $2 AND s.collected_at <= $3
		AND s.metrics_json ?| ARRAY['containerStorage','linux','k8s'] ORDER BY s.collected_at DESC,s.id DESC LIMIT 1`, id, from, to)
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
	forecasts := []map[string]any{}
	if len(latest) > 0 {
		recent = latest[0]
	}
	if recent != nil && params.Get("forecast") == "1" {
		var latestSample struct {
			At      time.Time
			Storage struct {
				PVCs []struct {
					UID   string
					Name  string
					Usage struct{ Status, SeriesKey string }
				}
			}
		}
		if err := json.Unmarshal(recent, &latestSample); err != nil {
			return nil, err
		}
		uids := []string{}
		for _, pvc := range latestSample.Storage.PVCs {
			if pvc.UID != "" && pvc.Name == params.Get("pvc") && (pvcUID == "" || pvcUID == pvc.UID) &&
				pvc.Usage.Status == "ok" && pvc.Usage.SeriesKey != "" {
				uids = append(uids, pvc.UID)
			}
		}
		if len(uids) > 0 {
			points, err := s.queryJSON(ctx, pvcCapacityHistorySQL, id, latestSample.At.Add(-7*24*time.Hour), latestSample.At, uids)
			if err != nil {
				return nil, err
			}
			observed := make([]pvcCapacityPoint, 0, len(points))
			for _, point := range points {
				var row pvcCapacityPoint
				if err := json.Unmarshal(point, &row); err != nil {
					return nil, err
				}
				observed = append(observed, row)
			}
			forecasts = pvcCapacityEstimates(observed)
		}
	}
	return map[string]any{"assetId": id, "from": from, "to": to, "stepSeconds": step,
		"buckets": rows, "latest": recent, "events": events, "pvcCapacity": forecasts}, nil
}

const pvcCapacityHistorySQL = `WITH recent AS (
	SELECT id,collected_at,metrics_json#>'{containerStorage,pvcs}' AS claims FROM samples
	WHERE asset_id=$1 AND collected_at >= $2 AND collected_at <= $3 ORDER BY collected_at DESC,id DESC LIMIT 16000
), points AS (
	SELECT id,collected_at,pvc->>'uid' AS uid,pvc->>'name' AS name,pvc#>>'{volume,uid}' AS volume_uid,
		pvc#>>'{usage,seriesKey}' AS source,pvc#>>'{usage,observedAt}' AS at,
		pvc#>'{usage,usedBytes}' AS used,pvc#>'{usage,capacityBytes}' AS capacity
	FROM recent CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(claims)='array'
		THEN claims ELSE '[]'::jsonb END) AS pvc
	WHERE pvc->>'uid'=ANY($4::text[]) AND pvc#>>'{usage,status}'='ok' AND pvc#>>'{usage,seriesKey}' IS NOT NULL
		AND pvc#>>'{usage,observedAt}' IS NOT NULL
		AND jsonb_typeof(pvc#>'{usage,usedBytes}')='number' AND jsonb_typeof(pvc#>'{usage,capacityBytes}')='number'
), observed AS (
	SELECT DISTINCT ON (uid,date_trunc('hour',at::timestamptz)) * FROM points
	ORDER BY uid,date_trunc('hour',at::timestamptz),at::timestamptz DESC,collected_at DESC,id DESC
)
SELECT jsonb_build_object('at',at,'sampleId',id,'uid',uid,'name',name,'volumeUid',volume_uid,'source',source,
	'used',used,'capacity',capacity) FROM observed ORDER BY at::timestamptz DESC,id DESC`

const containerHistorySQL = `WITH observations AS (
	SELECT date_bin(make_interval(secs=>$6::integer),collected_at,$8::timestamptz) AS bucket,
		metrics_json#>'{containerStorage}' AS storage, metrics_json AS metrics
	FROM samples WHERE asset_id=$1 AND collected_at >= $2 AND collected_at <= $3
	AND metrics_json ?| ARRAY['containerStorage','linux','k8s']
), selected AS (
	SELECT bucket, storage, metrics, storage->'containers'->coalesce(nullif($4,''), storage->>'primary') AS c
	FROM observations
), points AS (
	SELECT bucket, c, storage, metrics, CASE WHEN $5='' THEN c->'rates' ELSE c->'deviceRates'->$5 END AS rate,
		CASE WHEN $7::text<>'' THEN jsonb_path_query_first(storage->'pvcs',
			'$[*] ? (@.uid == $uid && @.usage.status == "ok")',jsonb_build_object('uid',$7::text))->'usage' END AS pvc,
		CASE WHEN metrics#>>'{diagnostics,activity,status}'='ok' AND jsonb_typeof(metrics#>'{diagnostics,activity,data}')='array' AND
			(metrics->'monitor_role'='true'::jsonb OR metrics->'stats_visible'='true'::jsonb) THEN
			(SELECT to_jsonb(coalesce(sum(a.sessions),0)) FROM jsonb_to_recordset(NULLIF(metrics#>'{diagnostics,activity,data}','null'::jsonb))
			AS a(state text,"waitType" text,sessions numeric) WHERE a.state='active' AND a."waitType"='IO') END AS pg_wait
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
		('pgIOWait', coalesce(storage->'pgIOWait', pg_wait)), ('pgReadTime',metrics#>'{diagnostics,database,data,readTimeMs}'),
		('tempBytes',metrics#>'{diagnostics,database,data,tempBytes}'), ('checkpointSync',metrics#>'{diagnostics,checkpoints,data,syncMs}'),
		('hostCpu',metrics#>'{linux,cpu,usedPercent}'), ('hostMemory',metrics#>'{linux,memory,usedPercent}'),
		('hostRead',metrics#>'{linux,interval,diskReadBps}'), ('hostWrite',metrics#>'{linux,interval,diskWriteBps}'),
		('hostRx',metrics#>'{linux,interval,networkReceiveBps}'), ('hostTx',metrics#>'{linux,interval,networkTransmitBps}'),
		('podCPU',metrics#>'{k8s,resourceMetrics,podUsage,cpuCores}'), ('podMemory',metrics#>'{k8s,resourceMetrics,podUsage,memoryBytes}'),
		('pvcUsed',pvc->'usedBytes'), ('pvcCapacity',pvc->'capacityBytes')
	) m(name,v) WHERE (c->>'status'='ok' OR m.name LIKE 'pg%' OR m.name LIKE 'host%' OR m.name LIKE 'pod%' OR m.name LIKE 'pvc%'
		OR m.name IN ('tempBytes','checkpointSync')) AND jsonb_typeof(m.v)='number'
	GROUP BY bucket, m.name
), bucket_series AS (
	SELECT bucket,jsonb_object_agg(name,jsonb_build_object('min',low,'max',high,'mean',mean,'count',n)) series
	FROM measures GROUP BY bucket
), coverage AS (
	SELECT bucket, count(*) AS samples, count(*) FILTER (WHERE c->>'status'='ok') AS available,
		count(*) FILTER (WHERE metrics#>>'{linux,status}'='ok') AS hosts,
		count(*) FILTER (WHERE metrics#>>'{k8s,status}'='ok') AS pods,
		count(*) FILTER (WHERE pvc IS NOT NULL) AS claims,
		bool_or(c->'identityChanged'='true'::jsonb) AS changed,
		bool_or(c->'counterReset'='true'::jsonb) AS reset
	FROM points GROUP BY bucket
)
SELECT jsonb_build_object('at',c.bucket,'samples',c.samples,'available',c.available,'identityChanged',c.changed,'counterReset',c.reset,
	'hosts',c.hosts,'pods',c.pods,'claims',c.claims,'series',coalesce(b.series,'{}'::jsonb))
FROM coverage c LEFT JOIN bucket_series b USING(bucket) ORDER BY c.bucket`
