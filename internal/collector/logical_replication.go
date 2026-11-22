package collector

const logicalPublicationsSQL = `WITH selected AS (
	SELECT oid,pubname,puballtables,pubinsert,pubupdate,pubdelete,pubtruncate,pubviaroot
	FROM pg_catalog.pg_publication ORDER BY pubname,oid LIMIT 40
), entries AS (
	SELECT p.oid::text AS oid,p.pubname AS name,p.puballtables AS "allTables",p.pubviaroot AS "viaRoot",
		p.pubinsert AS insert,p.pubupdate AS update,p.pubdelete AS delete,p.pubtruncate AS truncate,
		(SELECT count(*) FROM pg_catalog.pg_publication_rel r WHERE r.prpubid=p.oid) AS "explicitTables"
	FROM selected p
)
SELECT jsonb_build_object('total',(SELECT count(*) FROM pg_catalog.pg_publication),
	'entries',coalesce(jsonb_agg(to_jsonb(entries) ORDER BY name),'[]'::jsonb)) FROM entries`

// The shared subscription catalog and statistics views need a database filter.
// Select the required columns explicitly because subconninfo can hold a password.
const logicalSubscriptionsSQL = `WITH current_subscriptions AS (
	SELECT oid,subname,subowner,subenabled,subslotname,subpublications FROM pg_catalog.pg_subscription
	WHERE subdbid=(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database())
), selected AS (
	SELECT oid,subname,subowner,subenabled,subslotname,subpublications FROM current_subscriptions ORDER BY subname,oid LIMIT 40
), entries AS (
	SELECT s.oid::text AS oid,s.subname AS name,s.subenabled AS enabled,s.subslotname AS slot,
		s.subpublications[1:32] AS publications,cardinality(s.subpublications) AS "publicationCount",
		CASE WHEN NOT s.subenabled THEN 'disabled' WHEN w.pid IS NOT NULL THEN 'running'
			WHEN pg_has_role(s.subowner,'USAGE') OR pg_has_role('pg_read_all_stats','USAGE') THEN 'worker_absent'
			ELSE 'worker_unknown' END AS state,
		w.pid AS "applyPid",w.received_lsn::text AS "receivedLsn",w.latest_end_lsn::text AS "reportedLsn",
		w.last_msg_send_time AS "lastMessageSentAt",w.last_msg_receipt_time AS "lastMessageAt",
		w.latest_end_time AS "lastReportedAt",
		CASE WHEN w.last_msg_receipt_time <= statement_timestamp() THEN
			extract(epoch FROM statement_timestamp()-w.last_msg_receipt_time) END AS "messageAgeSeconds",
		(SELECT count(*) FROM pg_catalog.pg_subscription_rel r WHERE r.srsubid=s.oid) AS "tableCount",
		(SELECT count(*) FROM pg_catalog.pg_subscription_rel r WHERE r.srsubid=s.oid AND r.srsubstate='r') AS "readyTableCount"
	FROM selected s LEFT JOIN LATERAL (
		SELECT x.pid,x.received_lsn,x.latest_end_lsn,x.last_msg_send_time,x.last_msg_receipt_time,x.latest_end_time
		FROM pg_catalog.pg_stat_subscription x WHERE x.subid=s.oid AND x.relid IS NULL AND x.pid IS NOT NULL
			AND (to_jsonb(x)->>'leader_pid') IS NULL AND coalesce(to_jsonb(x)->>'worker_type','apply')='apply'
		ORDER BY x.pid LIMIT 1
	) w ON true
)
SELECT jsonb_build_object('total',(SELECT count(*) FROM current_subscriptions),
	'entries',coalesce(jsonb_agg(to_jsonb(entries) ORDER BY name),'[]'::jsonb)) FROM entries`

const logicalApplyProgressSQL = `SELECT coalesce(jsonb_agg(to_jsonb(q) ORDER BY q.oid),'[]'::jsonb) FROM (
	SELECT s.oid::text AS oid,o.remote_lsn::text AS "appliedOriginLsn",o.local_lsn::text AS "localCommitLsn"
	FROM pg_catalog.pg_subscription s JOIN pg_catalog.pg_replication_origin_status o
		ON o.external_id='pg_'||s.oid::text
	WHERE s.subdbid=(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database())
	ORDER BY s.oid LIMIT 40
) q`

const logicalSubscriptionErrorsSQL = `SELECT coalesce(jsonb_agg(to_jsonb(q) ORDER BY q.oid),'[]'::jsonb) FROM (
	SELECT s.oid::text AS oid,d.apply_error_count AS "applyErrors",
		coalesce(to_jsonb(d)->>'sync_table_error_count',to_jsonb(d)->>'sync_error_count')::bigint AS "syncErrors",
		(to_jsonb(d)->>'sync_seq_error_count')::bigint AS "sequenceSyncErrors",
		d.stats_reset AS "statsReset"
	FROM pg_catalog.pg_subscription s JOIN pg_catalog.pg_stat_subscription_stats d ON d.subid=s.oid
	WHERE s.subdbid=(SELECT oid FROM pg_catalog.pg_database WHERE datname=current_database())
	ORDER BY s.oid LIMIT 40
) q`

const logicalSlotPressureSQL = `SELECT coalesce((SELECT to_jsonb(q) FROM (
	SELECT slot_name AS name,plugin,active,wal_status AS "walStatus",restart_lsn::text AS "restartLsn",
		confirmed_flush_lsn::text AS "confirmedFlushLsn",
		greatest(0,pg_wal_lsn_diff(pg_current_wal_lsn(),restart_lsn))::text AS "retainedBytes"
	FROM pg_catalog.pg_replication_slots
	WHERE database=current_database() AND slot_type='logical' AND restart_lsn IS NOT NULL AND NOT temporary
	ORDER BY restart_lsn,slot_name LIMIT 1
) q),jsonb_build_object('retainedBytes','0'))`
