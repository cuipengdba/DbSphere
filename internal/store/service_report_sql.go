package store

// Use half-open intervals. A reading establishes coverage from its timestamp to
// the next reading, capped at twice the configured collection interval.
const serviceReportSQL = `WITH targets AS MATERIALIZED (
	SELECT a.id,a.name,a.organization_id,o.name AS organization,a.cluster_name,a.tags,a.runtime,
		make_interval(secs=>2*greatest(10,least(3600,coalesce(nullif(a.remote_interval_seconds,0),$4::int)))) AS freshness
	FROM assets a JOIN organizations o ON o.id=a.organization_id WHERE a.id=ANY($1::uuid[])
), report_window AS (
	SELECT tstzrange($2::timestamptz,$3::timestamptz,'[)') AS bounds
), readings AS MATERIALIZED (
	SELECT t.id AS asset_id,s.id,s.collected_at,s.status,
		CASE t.runtime WHEN 'mysql' THEN s.metrics_json#>>'{collector,mysqlStatus}'
		WHEN 'redis' THEN s.metrics_json#>>'{collector,redisStatus}'
		WHEN 'etcd' THEN s.metrics_json#>>'{collector,etcdStatus}'
		WHEN 'dm8' THEN s.metrics_json#>>'{collector,dm8Status}'
		WHEN 'opengauss' THEN s.metrics_json#>>'{collector,opengaussStatus}'
		WHEN 'kingbase' THEN s.metrics_json#>>'{collector,kingbaseStatus}'
		ELSE s.metrics_json#>>'{collector,pgStatus}' END AS target_status,
		CASE WHEN s.metrics_json->>'database_bytes' ~ '^[0-9]{1,19}$' THEN (s.metrics_json->>'database_bytes')::numeric END AS database_bytes,
		least(s.collected_at+t.freshness,lead(s.collected_at,1,$3) OVER (PARTITION BY t.id ORDER BY s.collected_at,s.id)) AS valid_until
	FROM targets t JOIN samples s ON s.asset_id=t.id
	WHERE s.collected_at >= $2::timestamptz-interval '2 hours' AND s.collected_at >= $2::timestamptz-t.freshness
	AND s.collected_at < $3::timestamptz
), reading_ranges AS (
	SELECT r.*,tstzrange(r.collected_at,r.valid_until,'[)') * w.bounds AS observed
	FROM readings r CROSS JOIN report_window w WHERE r.valid_until>r.collected_at
), observed AS (
	SELECT asset_id,coalesce(range_agg(observed) FILTER (WHERE status='ok' OR target_status='ok'),'{}') AS available,
		coalesce(range_agg(observed) FILTER (WHERE status<>'ok' AND target_status='availability'),'{}') AS unavailable
	FROM reading_ranges GROUP BY asset_id
), maintenance AS (
	SELECT t.id AS asset_id,range_agg(w.bounds*tstzrange(m.starts_at,least(m.ends_at,coalesce(m.revoked_at,m.ends_at)),'[)')) AS spans
	FROM targets t CROSS JOIN report_window w JOIN silences m ON (m.asset_id=t.id OR m.asset_id IS NULL)
	WHERE m.kind='maintenance' AND m.rule_id IS NULL AND m.starts_at<$3::timestamptz
		AND least(m.ends_at,coalesce(m.revoked_at,m.ends_at))>greatest(m.starts_at,$2::timestamptz)
	GROUP BY t.id
), classified AS (
	SELECT t.*,coalesce(m.spans,'{}') AS maintenance,
		coalesce(o.available,'{}')-coalesce(m.spans,'{}') AS available,
		coalesce(o.unavailable,'{}')-coalesce(m.spans,'{}') AS unavailable,
		tstzmultirange(w.bounds)-coalesce(m.spans,'{}')-coalesce(o.available,'{}')-coalesce(o.unavailable,'{}') AS missing
	FROM targets t CROSS JOIN report_window w LEFT JOIN maintenance m ON m.asset_id=t.id LEFT JOIN observed o ON o.asset_id=t.id
), totals AS (
	SELECT c.id,c.name,c.organization,c.organization_id,c.cluster_name,c.tags,
		coalesce((SELECT sum(extract(epoch FROM upper(r)-lower(r))) FROM unnest(c.available) r),0) AS available,
		coalesce((SELECT sum(extract(epoch FROM upper(r)-lower(r))) FROM unnest(c.unavailable) r),0) AS unavailable,
		coalesce((SELECT sum(extract(epoch FROM upper(r)-lower(r))) FROM unnest(c.maintenance) r),0) AS maintenance,
		coalesce((SELECT sum(extract(epoch FROM upper(r)-lower(r))) FROM unnest(c.missing) r),0) AS missing
	FROM classified c
), daily AS (
	SELECT DISTINCT ON (asset_id,day) asset_id,date_trunc('day',collected_at,'UTC') AS day,id,collected_at,database_bytes
	FROM readings WHERE collected_at >= $2::timestamptz AND status='ok' AND database_bytes IS NOT NULL
	ORDER BY asset_id,day,collected_at DESC,id DESC
), incidents AS MATERIALIZED (
	SELECT e.id,e.asset_id,e.title,e.event_group,e.started_at,e.acknowledged_at,e.ended_at,
		greatest(e.started_at,$2::timestamptz) AS clipped_start,least(coalesce(e.ended_at,$3),$3) AS clipped_end
	FROM events e JOIN targets t ON t.id=e.asset_id WHERE e.started_at<$3::timestamptz
	AND (e.ended_at IS NULL OR e.ended_at>$2::timestamptz)
), sample_refs AS (
	SELECT i.*,start_ref.id AS trigger_sample,end_ref.id AS recovery_sample,
		previous_ok.id AS last_good_sample,
		extract(epoch FROM i.started_at-previous_ok.collected_at) AS detection_upper_bound_seconds,
		CASE WHEN i.acknowledged_at BETWEEN i.started_at AND $3::timestamptz THEN extract(epoch FROM i.acknowledged_at-i.started_at) END AS acknowledge_seconds,
		CASE WHEN i.ended_at BETWEEN i.started_at AND $3::timestamptz THEN extract(epoch FROM i.ended_at-i.started_at) END AS recovery_seconds
	FROM (SELECT * FROM incidents ORDER BY started_at DESC,id LIMIT 100) i
	LEFT JOIN LATERAL (SELECT payload_json->>'sampleId' AS id, observed_at FROM evidence v WHERE v.event_id=i.id
		AND v.kind IN ('collection_error','alert_firing','alert_suppressed') AND v.payload_json->>'sampleId' IS NOT NULL
		ORDER BY v.observed_at,v.id LIMIT 1) start_ref ON true
	LEFT JOIN LATERAL (SELECT payload_json->>'sampleId' AS id FROM evidence v WHERE v.event_id=i.id
		AND v.kind IN ('collection_recovered','alert_recovered','recovery_verified') AND v.payload_json->>'sampleId' IS NOT NULL
		AND v.observed_at < $3::timestamptz ORDER BY v.observed_at DESC,v.id DESC LIMIT 1) end_ref ON true
	LEFT JOIN LATERAL (SELECT r.id,r.collected_at FROM reading_ranges r WHERE r.asset_id=i.asset_id
		AND i.event_group='availability' AND start_ref.observed_at=i.started_at
		AND r.collected_at<i.started_at AND r.valid_until=i.started_at AND r.target_status='ok'
		AND EXISTS(SELECT FROM readings failed WHERE failed.asset_id=i.asset_id AND failed.collected_at=i.started_at
			AND failed.id::text=start_ref.id AND failed.target_status='availability')
		ORDER BY r.collected_at DESC,r.id DESC LIMIT 1) previous_ok ON true
), combined AS (
	SELECT coalesce(sum(available),0) AS available,coalesce(sum(unavailable),0) AS unavailable,
		coalesce(sum(maintenance),0) AS maintenance,coalesce(sum(missing),0) AS missing FROM totals
)
SELECT jsonb_build_object('from',$2::timestamptz,'to',$3::timestamptz,'assetCount',(SELECT count(*) FROM targets),
	'collectionIntervalSeconds',$4::int,'availableSeconds',available,'unavailableSeconds',unavailable,
	'maintenanceSeconds',maintenance,'missingSeconds',missing,
	'observedAvailabilityPercent',round(100*available/nullif(available+unavailable,0),4),
	'coveragePercent',round(100*(available+unavailable)/nullif(available+unavailable+missing,0),4),
	'incidentCount',(SELECT count(*) FROM incidents),
	'assets',coalesce((SELECT jsonb_agg(jsonb_build_object('id',t.id,'name',t.name,'organization',t.organization,
		'organizationId',t.organization_id,'assetGroup',t.cluster_name,'tags',t.tags,'availableSeconds',t.available,
		'unavailableSeconds',t.unavailable,'maintenanceSeconds',t.maintenance,'missingSeconds',t.missing,
		'capacity',coalesce((SELECT jsonb_agg(jsonb_build_object('sampleId',d.id,'at',d.collected_at,'bytes',d.database_bytes) ORDER BY d.day)
		FROM daily d WHERE d.asset_id=t.id),'[]')) ORDER BY t.organization,t.name,t.id) FROM totals t),'[]'),
	'incidents',coalesce((SELECT jsonb_agg(jsonb_build_object('id',i.id,'assetId',i.asset_id,'title',i.title,
		'group',i.event_group,'detectedAt',i.started_at,'acknowledgedAt',CASE WHEN i.acknowledged_at<=$3 THEN i.acknowledged_at END,
		'recoveredAt',CASE WHEN i.ended_at<=$3 THEN i.ended_at END,'durationInWindowSeconds',extract(epoch FROM i.clipped_end-i.clipped_start),
		'acknowledgeSeconds',i.acknowledge_seconds,'recoverySeconds',i.recovery_seconds,
		'lastGoodSampleId',i.last_good_sample,'detectionUpperBoundSeconds',i.detection_upper_bound_seconds,
		'triggerSampleId',i.trigger_sample,'recoverySampleId',i.recovery_sample) ORDER BY i.started_at DESC,i.id) FROM sample_refs i),'[]'))
FROM combined`
