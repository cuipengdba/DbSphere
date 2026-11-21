package collector

const databaseInventorySQL = `SELECT jsonb_build_object('databases',coalesce(jsonb_agg(to_jsonb(q) ORDER BY q.connected DESC,q.name),'[]'::jsonb),
	'total',(SELECT count(*) FROM pg_database WHERE datallowconn AND NOT datistemplate AND has_database_privilege(oid,'CONNECT'))) FROM (
	SELECT d.oid::text AS oid,d.datname AS name,d.datname=current_database() AS connected,
		pg_database_size(d.oid) AS bytes,s.numbackends AS sessions,s.xact_commit AS commits,s.xact_rollback AS rollbacks,
		s.blks_read AS "blocksRead",s.blks_hit AS "blocksHit",s.tup_returned AS "rowsReturned",s.tup_fetched AS "rowsFetched",
		s.tup_inserted AS "rowsInserted",s.tup_updated AS "rowsUpdated",s.tup_deleted AS "rowsDeleted",
		s.deadlocks,s.temp_files AS "tempFiles",s.temp_bytes AS "tempBytes",s.stats_reset AS "statsReset"
	FROM pg_database d LEFT JOIN pg_stat_database s ON s.datid=d.oid
	WHERE d.datallowconn AND NOT d.datistemplate AND has_database_privilege(d.oid,'CONNECT')
	ORDER BY connected DESC,s.numbackends DESC NULLS LAST,d.datname LIMIT 32
) q`

const tableInventorySQL = `WITH stats AS MATERIALIZED (
	SELECT s.*,c.relpages,c.relfilenode,io.heap_blks_read,io.heap_blks_hit
	FROM pg_stat_user_tables s JOIN pg_class c ON c.oid=s.relid
	LEFT JOIN pg_statio_user_tables io ON io.relid=s.relid
	WHERE pg_has_role(current_user,'pg_read_all_stats','member') OR
		(has_schema_privilege(c.relnamespace,'USAGE') AND has_table_privilege(c.oid,'SELECT'))
), candidates AS (
	(SELECT relid FROM stats ORDER BY relpages DESC,relid LIMIT 20) UNION
	(SELECT relid FROM stats ORDER BY n_tup_ins+n_tup_upd+n_tup_del+seq_tup_read+idx_tup_fetch DESC NULLS LAST,relid LIMIT 20) UNION
	(SELECT relid FROM stats ORDER BY n_dead_tup DESC,relid LIMIT 20)
), measured AS (
	SELECT s.relid::text AS oid,s.relfilenode::text AS filenode,s.schemaname AS schema,s.relname AS name,
		pg_total_relation_size(s.relid) AS bytes,pg_indexes_size(s.relid) AS "indexBytes",
		s.n_live_tup AS "liveRows",s.n_dead_tup AS "deadRows",s.n_tup_ins+s.n_tup_upd+s.n_tup_del AS writes,
		s.seq_scan AS "seqScans",s.idx_scan AS "indexScans",s.heap_blks_read AS "blocksRead",s.heap_blks_hit AS "blocksHit",
		greatest(s.last_vacuum,s.last_autovacuum) AS "lastVacuumAt",greatest(s.last_analyze,s.last_autoanalyze) AS "lastAnalyzeAt"
	FROM candidates c JOIN stats s USING(relid)
)
SELECT jsonb_build_object('total',(SELECT count(*) FROM stats),
	'objects',coalesce(jsonb_agg(to_jsonb(m) ORDER BY m.bytes DESC NULLS LAST,m.oid),'[]'::jsonb)) FROM measured m`

const indexInventorySQL = `WITH candidates AS MATERIALIZED (
	SELECT s.*,c.relfilenode,c.relpages,io.idx_blks_read,io.idx_blks_hit
	FROM pg_stat_user_indexes s JOIN pg_class c ON c.oid=s.indexrelid
	LEFT JOIN pg_statio_user_indexes io ON io.indexrelid=s.indexrelid
	WHERE pg_has_role(current_user,'pg_read_all_stats','member') OR
		(has_schema_privilege(c.relnamespace,'USAGE') AND has_table_privilege(s.relid,'SELECT'))
), selected AS (
	(SELECT indexrelid FROM candidates ORDER BY relpages DESC,indexrelid LIMIT 24) UNION
	(SELECT indexrelid FROM candidates ORDER BY idx_scan DESC NULLS LAST,indexrelid LIMIT 24)
), measured AS (
	SELECT c.indexrelid::text AS oid,c.relfilenode::text AS filenode,c.schemaname AS schema,c.indexrelname AS name,
		c.relname AS "table",pg_relation_size(c.indexrelid) AS bytes,c.idx_scan AS scans,
		c.idx_tup_read AS "rowsRead",c.idx_blks_read AS "blocksRead",c.idx_blks_hit AS "blocksHit"
	FROM candidates c JOIN selected s USING(indexrelid)
)
SELECT jsonb_build_object('total',(SELECT count(*) FROM candidates),
	'objects',coalesce(jsonb_agg(to_jsonb(m) ORDER BY m.bytes DESC NULLS LAST,m.oid),'[]'::jsonb)) FROM measured m`

const instanceIOSQL = `SELECT to_jsonb(q) FROM (
	SELECT sum(reads) AS reads,sum(writes) AS writes,sum(fsyncs) AS fsyncs,max(stats_reset) AS "statsReset"
	FROM pg_stat_io WHERE backend_type='client backend' AND object='relation'
) q`
