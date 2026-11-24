package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
)

func migrateLogReport(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=54)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE log_records ADD COLUMN object_key text NOT NULL DEFAULT '',
		ADD COLUMN checkpoint_buffers bigint; INSERT INTO dbsphere_schema_migrations(version) VALUES (54)`)
	return err
}

const logReportFilter = `r.batch_id=$1 AND ($2='' OR r.application=$2)
	AND ($3::timestamptz IS NULL OR r.observed_at >= $3)
	AND ($4::timestamptz IS NULL OR r.observed_at < $4)
	AND ($5='' OR r.fingerprint=$5) AND ($6='' OR r.kind=$6)`

func (s *Postgres) LogReport(ctx context.Context, batch string, filters LogRecordQuery) (any, error) {
	if len(filters.Application) > 256 || len(filters.Fingerprint) > 64 || len(filters.Kind) > 32 {
		return nil, ErrLogInput
	}
	var coverage, engine string
	var asset *string
	err := s.pool.QueryRow(ctx, `SELECT b.coverage,i.engine,i.asset_id::text`+logBatchScope+
		`WHERE b.id=$1 AND ($2='' OR p.organization_id::text=$2)`, batch, OrganizationFromContext(ctx)).
		Scan(&coverage, &engine, &asset)
	if err != nil {
		return nil, logWriteError(err)
	}
	var summary json.RawMessage
	var from, to *time.Time
	args := []any{batch, filters.Application, filters.From, filters.To, filters.Fingerprint, filters.Kind}
	err = s.pool.QueryRow(ctx, `SELECT min(observed_at),max(observed_at),jsonb_build_object(
		'records',count(*),'valid',count(observed_at),
		'statements',count(*) FILTER(WHERE kind='statement'),
		'timed',count(duration_ms) FILTER(WHERE kind='statement'),
		'explicitTransactions',count(*) FILTER(WHERE kind='statement' AND command IN ('COMMIT','ROLLBACK','END')),
		'errors',count(*) FILTER(WHERE kind IN ('error','deadlock')),
		'latencyBuckets',jsonb_build_array(
			count(*) FILTER(WHERE kind='statement' AND duration_ms < 10),
			count(*) FILTER(WHERE kind='statement' AND duration_ms >= 10 AND duration_ms < 50),
			count(*) FILTER(WHERE kind='statement' AND duration_ms >= 50 AND duration_ms < 100),
			count(*) FILTER(WHERE kind='statement' AND duration_ms >= 100 AND duration_ms < 500),
			count(*) FILTER(WHERE kind='statement' AND duration_ms >= 500 AND duration_ms < 1000),
			count(*) FILTER(WHERE kind='statement' AND duration_ms >= 1000 AND duration_ms < 5000),
			count(*) FILTER(WHERE kind='statement' AND duration_ms >= 5000)),
		'p95',percentile_cont(0.95) WITHIN GROUP(ORDER BY duration_ms) FILTER(WHERE kind='statement'),
		'p99',percentile_cont(0.99) WITHIN GROUP(ORDER BY duration_ms) FILTER(WHERE kind='statement'))
		FROM log_records r WHERE `+logReportFilter, args...).
		Scan(&from, &to, &summary)
	if err != nil {
		return nil, err
	}
	report := map[string]any{"engine": engine, "coverage": coverage, "summary": summary, "assetId": asset,
		"series": []any{}, "applications": []any{}, "slowSQL": []any{}, "locks": []any{}, "bucketSeconds": 60,
		"examples": []any{}}
	if from == nil || to == nil {
		return report, nil
	}
	seconds := max(60, int((to.Sub(*from)+120*time.Second-1)/(120*time.Second)))
	anchor := from.Truncate(time.Minute)
	series, err := s.queryJSON(ctx, `SELECT jsonb_build_object('at',bucket,
		'read',count(*) FILTER(WHERE kind='statement' AND command IN('SELECT','SHOW','EXPLAIN')),
		'write',count(*) FILTER(WHERE kind='statement' AND command IN('INSERT','UPDATE','DELETE','MERGE','REPLACE','COPY')),
		'statements',count(*) FILTER(WHERE kind='statement'),
		'transactions',count(*) FILTER(WHERE kind='statement' AND command IN('COMMIT','ROLLBACK','END')),
		'lock',count(*) FILTER(WHERE kind='lock'),'deadlock',count(*) FILTER(WHERE kind='deadlock'),
		'error',count(*) FILTER(WHERE kind='error'),
		'checkpoint',count(*) FILTER(WHERE kind='checkpoint'),'checkpointBuffers',sum(checkpoint_buffers),
		'autovacuum',count(*) FILTER(WHERE kind='autovacuum'),
		'tempBytes',sum(temp_bytes),'temp',count(*) FILTER(WHERE kind='temp'),
		'p95',percentile_cont(0.95) WITHIN GROUP(ORDER BY duration_ms) FILTER(WHERE kind='statement'),
		'p99',percentile_cont(0.99) WITHIN GROUP(ORDER BY duration_ms) FILTER(WHERE kind='statement'))
		FROM (SELECT r.*,date_bin(make_interval(secs=>$7),r.observed_at,$8::timestamptz) AS bucket
			FROM log_records r WHERE r.observed_at IS NOT NULL AND `+logReportFilter+`) points
		GROUP BY bucket ORDER BY bucket`, append(args, seconds, anchor)...)
	if err != nil {
		return nil, err
	}
	apps, err := s.queryJSON(ctx, `SELECT jsonb_build_object('application',application,'records',count(*),
		'statements',count(*) FILTER(WHERE kind='statement'),'sqlMs',sum(duration_ms) FILTER(WHERE kind='statement'),
		'p95',percentile_cont(0.95) WITHIN GROUP(ORDER BY duration_ms) FILTER(WHERE kind='statement'),
		'waitMs',sum(duration_ms) FILTER(WHERE kind='lock'),'tempBytes',sum(temp_bytes),
		'errors',count(*) FILTER(WHERE kind IN ('error','deadlock')))
		FROM log_records r WHERE kind<>'unknown' AND `+logReportFilter+`
		GROUP BY application ORDER BY sum(duration_ms) FILTER(WHERE kind='statement') DESC NULLS LAST,count(*) DESC,application LIMIT 40`, args...)
	if err != nil {
		return nil, err
	}
	slow, err := s.queryJSON(ctx, `SELECT jsonb_build_object('fingerprint',fingerprint,'sql',min(sql_shape),
		'calls',count(*),'slowCalls',count(*) FILTER(WHERE duration_ms>=1000),
		'totalMs',sum(duration_ms),'maxMs',max(duration_ms),
		'p95',percentile_cont(0.95) WITHIN GROUP(ORDER BY duration_ms))
		FROM log_records r WHERE kind='statement' AND fingerprint<>'' AND duration_ms IS NOT NULL AND `+logReportFilter+`
		GROUP BY fingerprint ORDER BY max(duration_ms) DESC,sum(duration_ms) DESC,fingerprint LIMIT 30`, args...)
	if err != nil {
		return nil, err
	}
	locks, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',r.id,'at',observed_at,
		'application',application,'waiter',process_id,'blocker',blocker_id,'database',database_name,
		'durationMs',duration_ms,'kind',r.kind,'sql',sql_shape,'object',object_key,'nearbyEvent',nearby.id)
		FROM log_records r LEFT JOIN LATERAL (
			SELECT e.id FROM events e WHERE e.asset_id=$7
				AND e.started_at BETWEEN r.observed_at-interval '2 minutes' AND r.observed_at+interval '1 minute'
				AND (e.kind LIKE '%lock%' OR e.kind LIKE '%wait%')
			ORDER BY abs(extract(epoch FROM e.started_at-r.observed_at)),e.id LIMIT 1
		) nearby ON true WHERE r.kind IN ('lock','deadlock') AND `+logReportFilter+`
		ORDER BY observed_at DESC NULLS LAST,r.id LIMIT 30`, append(args, asset)...)
	if err != nil {
		return nil, err
	}
	var counters json.RawMessage
	if asset != nil && filters.Application == "" && filters.Fingerprint == "" && filters.Kind == "" {
		// A whole-instance TPS is only returned when the same database counter
		// and restart identity can be verified across samples in the log window.
		err = s.pool.QueryRow(ctx, `WITH observed AS (
			SELECT collected_at,metrics_json AS m FROM samples WHERE asset_id=$1 AND status='ok'
			AND collected_at BETWEEN $2 AND $3 AND metrics_json ? 'xact_commit' AND metrics_json ? 'xact_rollback'
			ORDER BY collected_at
		), a AS (SELECT * FROM observed ORDER BY collected_at LIMIT 1),
		b AS (SELECT * FROM observed ORDER BY collected_at DESC LIMIT 1)
		SELECT jsonb_build_object('databaseTps',CASE WHEN b.collected_at>a.collected_at
			AND a.m->>'postgresStartedAt' IS NOT NULL AND a.m->>'postgresStartedAt'=b.m->>'postgresStartedAt'
			AND a.m->>'databaseName'=b.m->>'databaseName' AND a.m->'postgresEndpoint'=b.m->'postgresEndpoint'
			AND a.m#>>'{diagnostics,database,data,statsReset}' IS NOT DISTINCT FROM b.m#>>'{diagnostics,database,data,statsReset}'
			AND (b.m->>'xact_commit')::numeric >= (a.m->>'xact_commit')::numeric
			AND (b.m->>'xact_rollback')::numeric >= (a.m->>'xact_rollback')::numeric
			THEN ((b.m->>'xact_commit')::numeric-(a.m->>'xact_commit')::numeric
				+(b.m->>'xact_rollback')::numeric-(a.m->>'xact_rollback')::numeric)
				/extract(epoch FROM (b.collected_at-a.collected_at)) END)
			FROM (VALUES (1)) x LEFT JOIN a ON true LEFT JOIN b ON true`, *asset, *from, *to).Scan(&counters)
		if err != nil {
			return nil, err
		}
		report["monitoringCounters"] = counters
	}
	if filters.Application != "" || filters.Fingerprint != "" {
		examples, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',r.id,'at',r.observed_at,'application',r.application,
			'sql',r.sql_shape,'durationMs',r.duration_ms,'retained',f.archive_expired_at IS NULL AND f.stored_size>0)
			FROM log_records r JOIN log_files f ON f.id=r.file_id WHERE r.kind='statement'
				AND r.duration_ms IS NOT NULL AND `+logReportFilter+
			` ORDER BY r.duration_ms DESC,r.observed_at DESC NULLS LAST,r.id LIMIT 3`, args...)
		if err != nil {
			return nil, err
		}
		report["examples"] = examples
	}
	report["series"], report["bucketSeconds"], report["from"], report["to"] = series, seconds, from, to
	report["applications"], report["slowSQL"], report["locks"] = apps, slow, locks
	return report, nil
}
