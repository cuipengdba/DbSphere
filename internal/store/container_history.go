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
	nodeDevice := params.Get("nodeDevice")
	if err != nil || len(name) > 63 || len(device) > 128 || len(pvcUID) > 128 || len(nodeDevice) > 128 || nodeDevice != "" && device != "" {
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
	rows, err := s.queryJSON(ctx, containerHistorySQL, id, from, to, name, device, step, pvcUID, origin, nodeDevice)
	if err != nil {
		return nil, err
	}
	latest, err := s.queryJSON(ctx, `SELECT jsonb_build_object('sampleId',s.id,'at',s.collected_at,
		'storage',s.metrics_json->'containerStorage','nodeRates',s.metrics_json#>'{linux,deviceRates,disk}',
		'nodeNumbers',s.metrics_json#>'{linux,deviceNumbers}',
		'host',jsonb_build_object('status',s.metrics_json#>'{linux,status}','cpu',s.metrics_json#>'{linux,cpu,usedPercent}',
			'memory',s.metrics_json#>'{linux,memory,usedPercent}','load',s.metrics_json#>'{linux,cpu,load1}',
			'network',s.metrics_json#>'{linux,interval}','boot',s.metrics_json#>'{linux,bootTimeSeconds}',
			'endpoint',s.metrics_json#>'{linux,endpoint}'),
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
	SELECT id,collected_at,date_bin(make_interval(secs=>$6::integer),collected_at,$8::timestamptz) AS bucket,
		metrics_json#>'{containerStorage}' AS storage, metrics_json AS metrics,
		lag(collected_at) OVER sample_order AS previous_at,lag(metrics_json) OVER sample_order AS previous
	FROM samples WHERE asset_id=$1 AND collected_at >= $2 AND collected_at <= $3
	AND metrics_json ?| ARRAY['containerStorage','linux','k8s']
	WINDOW sample_order AS (ORDER BY collected_at,id)
), selected AS (
	SELECT *, storage->'containers'->coalesce(nullif($4,''), storage->>'primary') AS c
	FROM observations
), points AS (
	SELECT selected.*,CASE WHEN $5='' THEN c->'rates' ELSE c->'deviceRates'->$5 END AS rate,
		metrics#>ARRAY['linux','deviceRates','disk',d.name] AS node_rate,
		CASE WHEN $7::text<>'' THEN jsonb_path_query_first(storage->'pvcs',
			'$[*] ? (@.uid == $uid && @.usage.status == "ok")',jsonb_build_object('uid',$7::text))->'usage' END AS pvc,
		CASE WHEN metrics#>>'{diagnostics,activity,status}'='ok' AND jsonb_typeof(metrics#>'{diagnostics,activity,data}')='array' AND
			(metrics->'monitor_role'='true'::jsonb OR metrics->'stats_visible'='true'::jsonb) THEN
			(SELECT to_jsonb(coalesce(sum(a.sessions),0)) FROM jsonb_to_recordset(NULLIF(metrics#>'{diagnostics,activity,data}','null'::jsonb))
			AS a(state text,"waitType" text,sessions numeric) WHERE a.state='active' AND a."waitType"='IO') END AS pg_wait
	FROM selected CROSS JOIN LATERAL (SELECT CASE
		WHEN metrics#>>'{linux,status}' IS DISTINCT FROM 'ok' THEN NULL
		WHEN $9::text<>'' AND c IS NULL AND metrics->'k8s' IS NULL AND coalesce(storage->>'runtime','')='' THEN $9
		WHEN $5<>'' AND c->>'status'='ok' THEN jsonb_path_query_first(c->'deviceLinks',
			'$[*] ? (@.device == $dev && @.scope == "node_device")',jsonb_build_object('dev',$5::text))->>'nodeDevice'
		END AS name) d
), identity AS (
	SELECT *, c->>'id' <> previous#>>ARRAY['containerStorage','containers',coalesce(nullif($4,''),storage->>'primary'),'id']
		OR c->>'identityChanged'='true' AS container_changed,
		coalesce(storage->>'podUID', metrics#>>'{k8s,pod,uid}') <>
			coalesce(previous#>>'{containerStorage,podUID}',previous#>>'{k8s,pod,uid}') AS pod_changed,
		coalesce(storage->>'nodeUID', metrics#>>'{k8s,node,uid}') <>
			coalesce(previous#>>'{containerStorage,nodeUID}',previous#>>'{k8s,node,uid}') AS node_changed,
		metrics#>>'{linux,bootTimeSeconds}' <> previous#>>'{linux,bootTimeSeconds}' AS host_rebooted
	FROM points
), pg_delta AS (
	SELECT bucket,m.name,to_jsonb(CASE WHEN m.rate THEN (numbers.current-numbers.prior)/extract(epoch FROM collected_at-previous_at)
		ELSE numbers.current-numbers.prior END) AS v
	FROM points CROSS JOIN LATERAL (VALUES
		('walBps','wal','bytes',true),('tempDelta','database','tempBytes',false),('checkpointDelta','checkpoints','syncMs',false)
	) m(name,topic,metric,rate)
	CROSS JOIN LATERAL (SELECT metrics#>ARRAY['diagnostics',m.topic,'data',m.metric] AS value,
		previous#>ARRAY['diagnostics',m.topic,'data',m.metric] AS before) p
	CROSS JOIN LATERAL (SELECT CASE WHEN jsonb_typeof(p.value)='number' OR (m.rate AND p.value#>>'{}' ~ '^[0-9]{1,24}$')
		THEN (p.value#>>'{}')::numeric END AS current,
		CASE WHEN jsonb_typeof(p.before)='number' OR (m.rate AND p.before#>>'{}' ~ '^[0-9]{1,24}$')
		THEN (p.before#>>'{}')::numeric END AS prior) numbers
	WHERE collected_at>previous_at AND collected_at-previous_at<=make_interval(secs=>greatest($6*2,300))
	AND metrics->>'postgresStartedAt' IS NOT NULL AND metrics->>'postgresStartedAt'=previous->>'postgresStartedAt'
	AND metrics->>'role' IS NOT DISTINCT FROM previous->>'role'
	AND metrics#>ARRAY['diagnostics',m.topic,'data','statsReset'] IS NOT DISTINCT FROM
		previous#>ARRAY['diagnostics',m.topic,'data','statsReset']
	AND numbers.current>=numbers.prior
), values_per_sample AS (
	SELECT bucket,m.name,m.v FROM points CROSS JOIN LATERAL (VALUES
		('nodeReadIops',node_rate->'reads'), ('nodeWriteIops',node_rate->'writes'),
		('nodeReadBps',node_rate->'readBytes'), ('nodeWriteBps',node_rate->'writtenBytes'),
		('queueDepth',node_rate->'weightedIOSeconds'),
		('nodeBusy',to_jsonb(100 * (node_rate->>'ioSeconds')::float8)),
		('nodeReadMs',to_jsonb(1000 * (node_rate->>'readSeconds')::float8 / nullif((node_rate->>'reads')::float8,0))),
		('nodeWriteMs',to_jsonb(1000 * (node_rate->>'writeSeconds')::float8 / nullif((node_rate->>'writes')::float8,0)))
	) m(name,v) WHERE jsonb_typeof(m.v)='number'
	UNION ALL SELECT * FROM pg_delta
), measures AS (
	SELECT bucket,name,min((v#>>'{}')::float8) AS low,max((v#>>'{}')::float8) AS high,avg((v#>>'{}')::float8) AS mean,count(*) AS n
	FROM (SELECT * FROM values_per_sample UNION ALL SELECT bucket,m.name,m.v FROM points CROSS JOIN LATERAL (VALUES
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
		OR m.name IN ('tempBytes','checkpointSync')) AND jsonb_typeof(m.v)='number') measured
	GROUP BY bucket,name
), bucket_series AS (
	SELECT bucket,jsonb_object_agg(name,jsonb_build_object('min',low,'max',high,'mean',mean,'count',n)) series
	FROM measures GROUP BY bucket
), coverage AS (
	SELECT bucket, count(*) AS samples, count(*) FILTER (WHERE c->>'status'='ok') AS available,
		count(*) FILTER (WHERE metrics#>>'{linux,status}'='ok') AS hosts,
		count(*) FILTER (WHERE metrics#>>'{k8s,status}'='ok') AS pods,
		count(*) FILTER (WHERE pvc IS NOT NULL) AS claims,
		(array_agg(id ORDER BY collected_at DESC,id DESC))[1] AS sample_id,max(collected_at) AS sample_at,
		coalesce(bool_or(container_changed),false) AS changed,
		coalesce(bool_or(pod_changed),false) AS pod_changed,coalesce(bool_or(node_changed),false) AS node_changed,
		coalesce(bool_or(host_rebooted),false) AS host_rebooted,coalesce(bool_or(c->>'counterReset'='true'),false) AS reset,
		(array_agg(id ORDER BY collected_at DESC,id DESC) FILTER(
			WHERE container_changed OR pod_changed OR node_changed OR host_rebooted OR c->>'counterReset'='true'))[1] AS change_sample
	FROM identity GROUP BY bucket
)
SELECT jsonb_build_object('at',c.bucket,'samples',c.samples,'available',c.available,'identityChanged',c.changed,'counterReset',c.reset,
	'podChanged',c.pod_changed,'nodeChanged',c.node_changed,'hostRebooted',c.host_rebooted,'changeSampleId',c.change_sample,
	'hosts',c.hosts,'pods',c.pods,'claims',c.claims,'latestSampleId',c.sample_id,'latestAt',c.sample_at,'series',coalesce(b.series,'{}'::jsonb))
FROM coverage c LEFT JOIN bucket_series b USING(bucket) ORDER BY c.bucket`
