package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

func (s *Postgres) databaseCatalog(ctx context.Context, id string, params url.Values) (any, error) {
	from, to, err := timeRange(params, 0, 7*24*time.Hour)
	minutes, invalidBucket := strconv.Atoi(params.Get("bucketMinutes"))
	database, object, kind := params.Get("dbOid"), params.Get("objectOid"), params.Get("objectKind")
	if err != nil || params.Get("to") == "" || invalidBucket != nil ||
		(minutes != 1 && minutes != 5 && minutes != 15 && minutes != 60) || to.Sub(from) > time.Duration(minutes*400)*time.Minute ||
		!validCatalogOID(database) || !validCatalogOID(object) ||
		(object != "" && (database == "" || (kind != "table" && kind != "index"))) || (object == "" && kind != "") {
		return nil, ErrInvalidRange
	}
	var snapshot json.RawMessage
	err = s.pool.QueryRow(ctx, `SELECT jsonb_build_object('id',id,'collectedAt',collected_at,'versionNum',metrics_json->'version_num',
		'databases',metrics_json#>'{diagnostics,databaseInventory}',
		'tables',metrics_json#>'{diagnostics,tableInventory}','indexes',metrics_json#>'{diagnostics,indexInventory}',
		'instanceIO',metrics_json#>'{diagnostics,instanceIO}')
		FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
		AND metrics_json#>>'{diagnostics,databaseInventory,status}'='ok'
		ORDER BY collected_at DESC,id DESC LIMIT 1`, id, from, to).Scan(&snapshot)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	growth, err := s.queryJSON(ctx, catalogGrowthSQL, id, from, to, database)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"snapshot": snapshot, "growth": growth, "series": []json.RawMessage{}, "objectSeries": []json.RawMessage{}}
	if database == "" {
		return result, nil
	}
	series, err := s.queryJSON(ctx, databaseSeriesSQL, id, from, to, minutes, database)
	if err != nil {
		return nil, err
	}
	result["series"] = series
	if object != "" {
		key := "tableInventory"
		if kind == "index" {
			key = "indexInventory"
		}
		result["objectSeries"], err = s.queryJSON(ctx, objectSeriesSQL, id, from, to, minutes, database, key, object)
	}
	return result, err
}

const catalogGrowthSQL = `WITH kinds(kind,inventory,items) AS (
	VALUES ('database','databaseInventory','databases'),('table','tableInventory','objects'),('index','indexInventory','objects')
), edges AS (
	SELECT k.kind,e.* FROM kinds k CROSS JOIN LATERAL (
		WITH eligible AS NOT MATERIALIZED (
			SELECT id,collected_at AS at,metrics_json#>ARRAY['diagnostics',k.inventory,'data',k.items] AS objects,
				db->>'oid' AS connected,db->>'statsReset' AS stats_reset,
				metrics_json#>>'{diagnostics,walPosition,data,startedAt}' AS started
			FROM samples CROSS JOIN LATERAL
				(SELECT jsonb_path_query_first(metrics_json#>'{diagnostics,databaseInventory,data,databases}','$[*] ? (@.connected == true)') AS db) connected
			WHERE asset_id=$1 AND collected_at >= $2 AND collected_at < $3
			AND metrics_json#>>ARRAY['diagnostics',k.inventory,'status']='ok'
			AND jsonb_typeof(metrics_json#>ARRAY['diagnostics',k.inventory,'data',k.items])='array'
			AND (k.kind='database' OR (db->>'oid' IS NOT NULL AND ($4='' OR db->>'oid'=$4)))
		)
		SELECT first.id AS first_id,first.at AS first_at,first.connected AS first_db,first.objects AS prior,
			last.id AS last_id,last.at AS last_at,last.connected AS last_db,last.objects AS current,
			first.stats_reset AS first_reset,last.stats_reset AS last_reset,first.started AS first_start,last.started AS last_start
		FROM (SELECT * FROM eligible ORDER BY at,id LIMIT 1) first
		CROSS JOIN (SELECT * FROM eligible ORDER BY at DESC,id DESC LIMIT 1) last
	) e
), paired AS (
	SELECT e.*,n AS latest,p AS previous,extract(epoch FROM last_at-first_at) AS seconds,
		first_id<>last_id AND (kind='database' OR first_db=last_db) AND p IS NOT NULL AS comparable
	FROM edges e CROSS JOIN LATERAL jsonb_array_elements(e.current) n
	LEFT JOIN LATERAL jsonb_path_query_first(e.prior,'$[*] ? (@.oid == $oid)',jsonb_build_object('oid',n->>'oid')) p ON true
), measured AS (
	SELECT *,CASE WHEN comparable THEN (latest->>'bytes')::bigint-(previous->>'bytes')::bigint END AS growth,
		comparable AND first_start IS NOT DISTINCT FROM last_start AND
		(CASE WHEN kind='database' THEN (latest->>'statsReset') IS NOT DISTINCT FROM (previous->>'statsReset')
			ELSE first_reset IS NOT DISTINCT FROM last_reset AND (latest->>'filenode') IS NOT DISTINCT FROM (previous->>'filenode') END) AS counters
	FROM paired
)
SELECT jsonb_build_object('kind',kind,'oid',latest->'oid','databaseOid',CASE WHEN kind='database' THEN latest->>'oid' ELSE last_db END,
	'firstSample',first_id,'lastSample',last_id,'firstAt',first_at,'lastAt',last_at,'seconds',CASE WHEN comparable THEN seconds END,
	'previousName',CASE WHEN comparable AND latest->>'name' <> previous->>'name' THEN previous->>'name' END,
	'rewritten',comparable AND latest->>'filenode'<>previous->>'filenode',
	'growthBytes',growth,
	'projectedGrowth7d',CASE WHEN growth>0 AND seconds>=86400 THEN round(growth * 604800.0 / seconds) END,
	'blocksReadDelta',CASE WHEN counters AND (latest->>'blocksRead')::bigint >= (previous->>'blocksRead')::bigint
		THEN (latest->>'blocksRead')::bigint-(previous->>'blocksRead')::bigint END,
	'writesDelta',CASE WHEN counters AND (latest->>'writes')::bigint >= (previous->>'writes')::bigint
		THEN (latest->>'writes')::bigint-(previous->>'writes')::bigint END,
	'scansDelta',CASE WHEN counters AND (latest->>'scans')::bigint >= (previous->>'scans')::bigint
		THEN (latest->>'scans')::bigint-(previous->>'scans')::bigint END,
	'previousDeadRows',CASE WHEN comparable THEN previous->'deadRows' END,
	'previousVacuumAt',CASE WHEN comparable THEN previous->'lastVacuumAt' END,
	'previousAnalyzeAt',CASE WHEN comparable THEN previous->'lastAnalyzeAt' END)
FROM measured ORDER BY kind,growth DESC NULLS LAST,latest->>'oid'`

func validCatalogOID(oid string) bool {
	if oid == "" {
		return true
	}
	num, err := strconv.ParseUint(oid, 10, 32)
	return err == nil && num > 0 && oid == strconv.FormatUint(num, 10)
}

const databaseSeriesSQL = `WITH samples_in_range AS (
	SELECT id,collected_at,metrics_json#>>'{diagnostics,walPosition,data,startedAt}' AS started,
		CASE WHEN metrics_json#>>'{diagnostics,databaseInventory,status}'='ok' THEN
			jsonb_path_query_first(metrics_json#>'{diagnostics,databaseInventory,data,databases}',
			'$[*] ? (@.oid == $oid)',jsonb_build_object('oid',$5::text)) END AS db
	FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
), compared AS (
	SELECT *,lag(db) OVER chronological AS prior,lag(started) OVER chronological AS prior_started,
		extract(epoch FROM collected_at-lag(collected_at) OVER chronological) AS seconds
	FROM samples_in_range WINDOW chronological AS (ORDER BY collected_at,id)
), deltas AS (
	SELECT date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz) AS bucket,
		db,key,(db->>key)::numeric - (prior->>key)::numeric AS diff,seconds,
		db IS NOT NULL AND prior IS NOT NULL AND db->>'statsReset' IS NOT DISTINCT FROM prior->>'statsReset'
		AND started IS NOT DISTINCT FROM prior_started AND seconds BETWEEN 1 AND 600 AS comparable
	FROM compared CROSS JOIN unnest(ARRAY['commits','rollbacks','blocksRead','blocksHit','rowsReturned','rowsFetched',
		'rowsInserted','rowsUpdated','rowsDeleted','deadlocks','tempFiles','tempBytes']) AS counts(key)
), grouped AS (
	SELECT bucket,key,sum(diff) FILTER (WHERE comparable AND diff>=0) AS delta,
		sum(seconds) FILTER (WHERE comparable AND diff>=0) AS seconds FROM deltas GROUP BY bucket,key
), counters AS (
	SELECT bucket,jsonb_object_agg(key,delta) AS deltas,jsonb_object_agg(key,round(delta/nullif(seconds,0),2)) AS rates
	FROM grouped GROUP BY bucket
), gauges AS (
	SELECT bucket,avg((db->>'sessions')::numeric) AS sessions,max((db->>'bytes')::numeric) AS bytes,
		count(db)::int AS observed FROM deltas WHERE key='commits' GROUP BY bucket
)
SELECT jsonb_build_object('bucket',bucket,'observed',observed,'sessions',round(sessions,1),'bytes',bytes,
	'transactionsPerSecond',(rates->>'commits')::numeric+(rates->>'rollbacks')::numeric,
	'cacheHitPercent',round(100*(deltas->>'blocksHit')::numeric/nullif((deltas->>'blocksHit')::numeric+(deltas->>'blocksRead')::numeric,0),2),
	'deltas',deltas,'rates',rates)
FROM counters JOIN gauges USING(bucket) ORDER BY bucket`

const objectSeriesSQL = `WITH measured AS (
	SELECT collected_at,id,date_bin(make_interval(mins=>$4),collected_at,'2000-01-01T00:00:00Z'::timestamptz) AS bucket,
		CASE WHEN jsonb_path_exists(metrics_json#>'{diagnostics,databaseInventory,data,databases}',
			'$[*] ? (@.oid == $oid && @.connected == true)',jsonb_build_object('oid',$5::text))
			THEN jsonb_path_query_first(metrics_json#>ARRAY['diagnostics',$6,'data','objects'],
				'$[*] ? (@.oid == $oid)',jsonb_build_object('oid',$7::text)) END AS object
	FROM samples WHERE asset_id=$1 AND collected_at>=$2 AND collected_at<$3
)
SELECT DISTINCT ON(bucket) jsonb_build_object('bucket',bucket,'sampleId',id,'collectedAt',collected_at,
	'bytes',object->'bytes','deadRows',object->'deadRows','blocksRead',object->'blocksRead',
	'modifiedSinceAnalyze',object->'modifiedSinceAnalyze','xidAge',object->'xidAge')
FROM measured ORDER BY bucket,collected_at DESC,id DESC`
