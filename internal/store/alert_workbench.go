package store

const alertWorkbenchSQL = `WITH recent AS (
	SELECT e.*,a.name AS asset_name,a.cluster_name,a.last_sample_at
	FROM events e JOIN assets a ON a.id=e.asset_id
	WHERE (e.status='open' OR e.ended_at>=now()-interval '72 hours') /*asset_scope*/
	ORDER BY (e.status='open') DESC,e.started_at DESC,e.id LIMIT 100
)
SELECT jsonb_build_object('id',e.id,'assetId',e.asset_id,'assetName',e.asset_name,'cluster',e.cluster_name,
	'title',e.title,'group',e.event_group,'status',e.status,'start',e.started_at,'end',e.ended_at,'ack',e.acknowledged_at,
	'lastSampleAt',e.last_sample_at,'severity',COALESCE(state.severity, signals.severity),
	'lastTrigger',signals.last_trigger,'sources',signals.sources,
	'activeSignals',state.total,'missingSignals',state.missing,'recoveringSignals',state.recovering,'mutedSignals',state.muted,
	'maintenance',signals.maintenance,'notifications',delivery.counts,'lastDelivery',delivery.last_delivery,
	'handoff',signals.handoff,'falsePositive',signals.false_positive,
	'owner',owner.username,'acceptedAt',handoff.accepted_at,
	'tasks',array_remove(ARRAY[
		CASE WHEN e.status='open' AND handoff.event_id IS NOT NULL AND handoff.accepted_at IS NULL THEN 'accept' END,
		CASE WHEN e.status='open' AND (handoff.accepted_at IS NOT NULL OR handoff.event_id IS NULL) THEN 'follow_up' END,
		CASE WHEN coalesce((delivery.counts->>'failed')::int,0)>0 THEN 'delivery_failed' END,
		CASE WHEN e.status='resolved' AND signals.recovery_verified IS NULL THEN 'verify_recovery' END],NULL))
FROM recent e
LEFT JOIN event_handoffs handoff ON handoff.event_id=e.id
LEFT JOIN access_accounts owner ON owner.id=handoff.account_id
CROSS JOIN LATERAL (
	SELECT count(*) AS total,count(*) FILTER (WHERE s.state_json->>'missingSince' IS NOT NULL) AS missing,
		count(*) FILTER (WHERE s.state_json->>'recoveringSince' IS NOT NULL) AS recovering,
		count(*) FILTER (WHERE s.state_json->>'mutedBy' IS NOT NULL) AS muted,
		(ARRAY['info','warning','critical'])[max(array_position(ARRAY['info','warning','critical'],
			coalesce(s.state_json->'firingRule'->>'severity',r.definition_json->>'severity')))] AS severity
	FROM alert_states s JOIN alert_rules r ON r.id=s.rule_id
	WHERE s.asset_id=e.asset_id AND s.state_json->>'eventId'=e.id::text AND s.state_json->>'status'='firing'
) state
CROSS JOIN LATERAL (
	SELECT max(x.observed_at) FILTER (WHERE x.kind IN ('alert_firing','alert_suppressed','alert_unsuppressed','collection_error')) AS last_trigger,
		(ARRAY['info','warning','critical'])[max(array_position(ARRAY['info','warning','critical'],x.payload_json->>'severity'))] AS severity,
		array_agg(DISTINCT x.payload_json->>'source' ORDER BY x.payload_json->>'source') FILTER (WHERE x.payload_json->>'source' IS NOT NULL) AS sources,
		max(x.observed_at) FILTER (WHERE x.kind='operator_response' AND x.payload_json->>'phase'='handoff') AS handoff,
		max(x.observed_at) FILTER (WHERE x.kind='operator_response' AND x.payload_json->>'phase'='false_positive') AS false_positive,
		max(x.observed_at) FILTER (WHERE x.kind='recovery_verified') AS recovery_verified,
		bool_or(x.kind='alert_suppressed' AND x.payload_json->>'silenceId' IS NOT NULL) AS maintenance
	FROM evidence x WHERE x.event_id=e.id
) signals
CROSS JOIN LATERAL (
	SELECT jsonb_object_agg(status,total) AS counts,max(sent) AS last_delivery FROM (
		SELECT n.status,count(*) AS total,max(n.delivered_at) FILTER (WHERE n.status='delivered') AS sent
		FROM evidence x JOIN notification_outbox n ON n.evidence_id=x.id WHERE x.event_id=e.id GROUP BY n.status
	) status_counts
) delivery ORDER BY (e.status='open') DESC,e.started_at DESC,e.id`
