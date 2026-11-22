package store

import (
	"context"
	"net/url"
	"strconv"
	"time"
)

func (s *Postgres) environmentHistory(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	minutes, bucketErr := strconv.Atoi(params.Get("bucketMinutes"))
	deviceType, device := params.Get("deviceType"), params.Get("device")
	if err != nil || params.Get("to") == "" || bucketErr != nil ||
		(minutes != 1 && minutes != 5 && minutes != 15 && minutes != 60) || to.Sub(from) > time.Duration(400*minutes)*time.Minute ||
		len(device) > 256 || deviceType == "" && device != "" ||
		deviceType != "" && (device == "" || deviceType != "disk" && deviceType != "network" && deviceType != "filesystem") {
		return nil, ErrInvalidRange
	}
	series, err := s.queryJSON(ctx, environmentSeriesSQL, id, from, to, minutes)
	if err != nil {
		return nil, err
	}
	changes, err := s.queryJSON(ctx, environmentChangesSQL, id, from, to)
	if err != nil {
		return nil, err
	}
	containers, err := s.queryJSON(ctx, containerSeriesSQL, id, from, to, minutes)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"series": series, "changes": changes, "containers": containers}
	if deviceType != "" {
		result["deviceSeries"], err = s.queryJSON(ctx, deviceHistorySQL, id, from, to, minutes, deviceType, device)
	}
	return result, err
}

const deviceHistorySQL = `WITH samples_in_range AS (
	SELECT id,collected_at,date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz) AS bucket,
		metrics_json#>>'{linux,endpoint}' AS endpoint,metrics_json#>>'{linux,bootTimeSeconds}' AS boot,
		CASE WHEN $5='filesystem' THEN jsonb_path_query_first(metrics_json#>'{linux,filesystems}',
			'$[*] ? (@.mountpoint == $mount)',jsonb_build_object('mount',$6::text))
			ELSE metrics_json#>ARRAY['linux','devices',$5,$6] END AS device,
		metrics_json#>ARRAY['linux','deviceRates',$5,$6] AS rates
	FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
)
SELECT DISTINCT ON(bucket) jsonb_build_object('bucket',bucket,'sampleId',id,'collectedAt',collected_at,
	'observed',device IS NOT NULL,'identity',CASE WHEN device IS NOT NULL THEN concat_ws('/',endpoint,boot,device->>'device') END,
	'usedPercent',device->'usedPercent','availableBytes',device->'availableBytes','sizeBytes',device->'sizeBytes',
	'readBps',rates->'readBytes','writeBps',rates->'writtenBytes',
	'busyPercent',100*(rates->>'ioSeconds')::numeric,'queueDepth',rates->'weightedIOSeconds',
	'readIOPS',rates->'reads','writeIOPS',rates->'writes',
	'receiveBps',rates->'receiveBytes','transmitBps',rates->'transmitBytes',
	'receiveErrors',rates->'receiveErrors','transmitErrors',rates->'transmitErrors',
	'receiveDrops',rates->'receiveDrops','transmitDrops',rates->'transmitDrops')
FROM samples_in_range ORDER BY bucket,collected_at DESC,id DESC`

const containerSeriesSQL = `WITH observed AS (
	SELECT date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz) AS bucket,
		metrics_json#>>'{k8s,pod,uid}' AS uid,metrics_json#>'{k8s,resourceMetrics,containers}' AS containers,
		metrics_json#>'{k8s,resourceSpecs}' AS specs
	FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
	AND metrics_json#>'{k8s,resourceMetrics,podUsage}' IS NOT NULL
), grouped AS (
	SELECT bucket,name,min(uid) AS uid,avg("cpuCores") AS cpu,avg("memoryBytes") AS memory,
		max((specs#>>ARRAY[name,'requests','cpuCores'])::numeric) AS cpu_request,
		max((specs#>>ARRAY[name,'limits','cpuCores'])::numeric) AS cpu_limit,
		max((specs#>>ARRAY[name,'requests','memoryBytes'])::numeric) AS memory_request,
		max((specs#>>ARRAY[name,'limits','memoryBytes'])::numeric) AS memory_limit
	FROM observed CROSS JOIN LATERAL jsonb_to_recordset(containers) AS c(name text,"cpuCores" numeric,"memoryBytes" numeric)
	WHERE uid IS NOT NULL AND name IS NOT NULL AND ("cpuCores" IS NOT NULL OR "memoryBytes" IS NOT NULL)
	GROUP BY bucket,name HAVING count(DISTINCT uid)=1
), chosen AS (
	SELECT name FROM grouped GROUP BY name ORDER BY count(*) DESC,name LIMIT 20
)
SELECT jsonb_build_object('name',name,'series',jsonb_agg(jsonb_build_object('bucket',bucket,'podUid',uid,
	'cpuCores',round(cpu,4),'memoryBytes',round(memory,0),'cpuRequest',cpu_request,'cpuLimit',cpu_limit,
	'memoryRequest',memory_request,'memoryLimit',memory_limit) ORDER BY bucket))
FROM grouped JOIN chosen USING(name) GROUP BY name ORDER BY name`

const environmentSeriesSQL = `WITH binned AS (
	SELECT date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz) AS bucket,
		metrics_json->'linux' AS linux,metrics_json->'k8s' AS kube,CASE WHEN status='ok' THEN 'ok'
		ELSE metrics_json#>>'{collector,pgStatus}' END AS pg
	FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
), sampled AS (
	SELECT bucket,pg,kube#>>'{pod,uid}' AS uid,
		(SELECT status FROM jsonb_to_recordset(NULLIF(kube#>'{pod,conditions}','null'::jsonb)) AS c(type text,status text) WHERE type='Ready' LIMIT 1) AS ready,
		(linux#>>'{cpu,usedPercent}')::numeric AS cpu,(linux#>>'{memory,usedPercent}')::numeric AS memory,
		(linux#>>'{cpu,ioWaitPercent}')::numeric AS cpu_iowait,
		linux->>'status'='ok' AS linux_available,kube->>'status'='ok' AS kube_available,
		(linux#>>'{cpu,load1}')::numeric AS load,
		(SELECT max("usedPercent") FROM jsonb_to_recordset(NULLIF(linux->'filesystems','null'::jsonb)) AS fs("usedPercent" numeric)) AS disk_used,
		(linux#>>'{interval,diskReadBps}')::numeric AS disk_read,(linux#>>'{interval,diskWriteBps}')::numeric AS disk_write,
		(linux#>>'{interval,networkReceiveBps}')::numeric AS network_rx,(linux#>>'{interval,networkTransmitBps}')::numeric AS network_tx,
		(linux#>>'{interval,cpuWait}')::numeric AS pressure_cpu,(linux#>>'{interval,memoryWait}')::numeric AS pressure_memory,
		(linux#>>'{interval,ioWait}')::numeric AS pressure_io,
		(kube#>>'{resourceMetrics,podUsage,cpuCores}')::numeric AS pod_cpu,
		(kube#>>'{resourceMetrics,podUsage,memoryBytes}')::numeric AS pod_memory
	FROM binned
)
SELECT jsonb_build_object('bucket',bucket,'observed',count(*)::int,'pgUnavailable',count(*) FILTER (WHERE pg='availability')::int,
	'pgAvailable',count(*) FILTER (WHERE pg='ok')::int,'podUid',CASE WHEN count(DISTINCT uid)=1 THEN min(uid) END,
	'podReady',count(*) FILTER (WHERE ready='True')::int,'podUnready',count(*) FILTER (WHERE ready='False')::int,
	'cpuPercent',round(avg(cpu),1),'cpuIOWait',round(avg(cpu_iowait),2),'memoryPercent',round(avg(memory),1),'cpuLoad',round(avg(load),2),
	'hostObserved',count(*) FILTER (WHERE linux_available),'kubeObserved',count(*) FILTER (WHERE kube_available),
	'resourceObserved',count(*) FILTER (WHERE pod_cpu IS NOT NULL OR pod_memory IS NOT NULL),
	'diskUsedPercent',round(max(disk_used),1),'diskReadBps',round(avg(disk_read),0),'diskWriteBps',round(avg(disk_write),0),
	'networkReceiveBps',round(avg(network_rx),0),'networkTransmitBps',round(avg(network_tx),0),
	'pressureCPU',round(avg(pressure_cpu)*100,2),'pressureMemory',round(avg(pressure_memory)*100,2),'pressureIO',round(avg(pressure_io)*100,2),
	'podCPUCores',CASE WHEN count(DISTINCT uid)=1 THEN round(avg(pod_cpu),4) END,
	'podMemoryBytes',CASE WHEN count(DISTINCT uid)=1 THEN round(avg(pod_memory),0) END)
FROM sampled GROUP BY bucket ORDER BY bucket`

const environmentChangesSQL = `WITH frames AS (
	SELECT id,collected_at,metrics_json->'k8s' AS kube,metrics_json->'linux' AS linux,
		CASE WHEN status='ok' THEN 'ok' ELSE metrics_json#>>'{collector,pgStatus}' END AS pg
	FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
), states AS (
	SELECT id,collected_at,pg,kube,linux,kube#>>'{pod,uid}' AS uid,kube#>>'{pod,name}' AS pod,kube#>>'{pod,node}' AS node,
		kube#>>'{node,uid}' AS node_uid,kube->'owner' AS owner,kube->'endpointRoutes' AS routes,
		linux->>'endpoint' AS host_endpoint,
		(SELECT status FROM jsonb_to_recordset(NULLIF(kube#>'{pod,conditions}','null'::jsonb)) AS c(type text,status text) WHERE type='Ready' LIMIT 1) AS ready,
		(SELECT status FROM jsonb_to_recordset(NULLIF(kube#>'{pod,conditions}','null'::jsonb)) AS c(type text,status text) WHERE type='PodScheduled' LIMIT 1) AS scheduled,
		(SELECT jsonb_object_agg(name,"restartCount") FROM jsonb_to_recordset(NULLIF(kube->'containers','null'::jsonb)) AS c(name text,"restartCount" int)
		 WHERE name IS NOT NULL) AS restarts,
		(SELECT string_agg(type,', ' ORDER BY type) FROM jsonb_to_recordset(NULLIF(kube#>'{node,conditions}','null'::jsonb)) AS c(type text,status text)
		 WHERE type IN ('MemoryPressure','DiskPressure','PIDPressure','NetworkUnavailable') AND status='True') AS node_pressure,
		(SELECT jsonb_object_agg(name,phase) FROM jsonb_to_recordset(NULLIF(kube->'pvcs','null'::jsonb)) AS c(name text,phase text) WHERE name IS NOT NULL) AS pvcs,
		(SELECT jsonb_object_agg(name,jsonb_build_array(uid,capacity,requested))
			FROM jsonb_to_recordset(NULLIF(kube->'pvcs','null'::jsonb)) AS c(name text,uid text,capacity text,requested text)
			WHERE name IS NOT NULL) AS volumes,
		(linux->>'bootTimeSeconds')::numeric AS boot
	FROM frames
), pod_identity AS (
	SELECT id,lag(uid) OVER (ORDER BY collected_at,id) AS previous_uid FROM states WHERE uid IS NOT NULL
), compared AS (
	SELECT states.*,pod_identity.previous_uid,lag(uid) OVER chronological AS prior_uid,lag(ready) OVER chronological AS prior_ready,
		lag(scheduled) OVER chronological AS prior_scheduled,lag(pg) OVER chronological AS prior_pg,
		lag(pvcs) OVER chronological AS prior_pvcs,lag(node_pressure) OVER chronological AS prior_pressure,
		lag(restarts) OVER chronological AS prior_restarts,lag(node) OVER chronological AS prior_node,
		lag(routes) OVER chronological AS prior_routes,lag(owner) OVER chronological AS prior_owner,
		lag(volumes) OVER chronological AS prior_volumes,lag(node_uid) OVER chronological AS prior_node_uid,
		lag(host_endpoint) OVER chronological AS prior_host_endpoint,
		lag(boot) OVER chronological AS prior_boot,row_number() OVER chronological AS n
	FROM states LEFT JOIN pod_identity USING(id) WINDOW chronological AS (ORDER BY collected_at,id)
)
SELECT jsonb_build_object('id',id,'collectedAt',collected_at,'pg',pg,'previousPG',prior_pg,'pod',pod,'uid',uid,'previousUID',previous_uid,
	'node',node,'ready',ready,'scheduled',scheduled,'restarts',restarts,'nodePressure',node_pressure,'pvcs',pvcs,
	'nodeUID',node_uid,'owner',owner,'endpointRoutes',routes,'volumes',volumes,'routesChanged',routes IS DISTINCT FROM prior_routes,
	'hostEndpoint',host_endpoint,'hostEndpointChanged',host_endpoint IS DISTINCT FROM prior_host_endpoint,
	'hostRestart',boot<>prior_boot,'events',kube->'events')
FROM compared WHERE n=1 OR pg IS DISTINCT FROM prior_pg OR uid IS DISTINCT FROM prior_uid OR ready IS DISTINCT FROM prior_ready
	OR scheduled IS DISTINCT FROM prior_scheduled OR restarts IS DISTINCT FROM prior_restarts OR node IS DISTINCT FROM prior_node
	OR node_pressure IS DISTINCT FROM prior_pressure OR pvcs IS DISTINCT FROM prior_pvcs OR boot<>prior_boot
	OR owner IS DISTINCT FROM prior_owner OR routes IS DISTINCT FROM prior_routes OR volumes IS DISTINCT FROM prior_volumes
	OR node_uid IS DISTINCT FROM prior_node_uid OR host_endpoint IS DISTINCT FROM prior_host_endpoint
ORDER BY collected_at DESC,id DESC LIMIT 60`
