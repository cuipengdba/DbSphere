package collector

// Read only active client sessions in the monitored database. An active
// session with no reported wait is runnable or running, so it is labeled
// NoWait rather than reported as measured CPU utilization.
const activeWaitsSQL = `WITH active AS (
	SELECT coalesce(wait_event_type,'NoWait') AS "waitType",
		coalesce(wait_event,'') AS "waitEvent",coalesce(query_id::text,'') AS "queryId"
	FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()
		AND backend_type='client backend' AND state='active'
), grouped AS (
	SELECT GROUPING("waitEvent","queryId") AS depth,"waitType","waitEvent","queryId",count(*)::int AS sessions
	FROM active GROUP BY GROUPING SETS (("waitType"),("waitType","waitEvent"),("waitType","waitEvent","queryId"))
), ranked AS (
	SELECT *,row_number() OVER(PARTITION BY depth ORDER BY sessions DESC,"waitType","waitEvent","queryId") AS pos,
		count(*) OVER(PARTITION BY depth) AS cardinality
	FROM grouped
)
SELECT CASE WHEN pg_has_role(current_user,'pg_read_all_stats','USAGE') THEN jsonb_build_object(
	'groups',coalesce(jsonb_agg(jsonb_build_object('depth',depth,'waitType',"waitType",'waitEvent',"waitEvent",
		'queryId',"queryId",'sessions',sessions) ORDER BY depth,pos)
		FILTER (WHERE pos<=80),'[]'::jsonb),
	'truncated',coalesce(bool_or(cardinality>80),false)) END
FROM ranked`
