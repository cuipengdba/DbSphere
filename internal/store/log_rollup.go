package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

func migrateLogArchiveManagement(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=56)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE log_batches ADD COLUMN archive_revision bigint NOT NULL DEFAULT 0;
		CREATE TABLE log_batch_rollups (
			batch_id uuid PRIMARY KEY REFERENCES log_batches(id) ON DELETE CASCADE, revision bigint NOT NULL,
			first_at timestamptz, last_at timestamptz, records bigint NOT NULL, scanned bigint NOT NULL,
			statements bigint NOT NULL, timed bigint NOT NULL, slow bigint NOT NULL, errors bigint NOT NULL,
			locks bigint NOT NULL, deadlocks bigint NOT NULL
		);
		CREATE TABLE log_issue_rollups (
			batch_id uuid NOT NULL REFERENCES log_batch_rollups(batch_id) ON DELETE CASCADE,
			kind text NOT NULL, issue text NOT NULL, database_name text NOT NULL, application text NOT NULL,
			label text NOT NULL, calls bigint NOT NULL, timed bigint NOT NULL, slow bigint NOT NULL,
			mean double precision, p95 double precision, total double precision, evidence_id uuid NOT NULL,
			PRIMARY KEY(batch_id,kind,issue,database_name,application)
		);
		ALTER TABLE log_files ADD COLUMN verified_at timestamptz, ADD COLUMN archive_expired_at timestamptz,
			ADD COLUMN processing_attempts integer NOT NULL DEFAULT 0,
			ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now();
		CREATE INDEX log_file_retry_queue ON log_files(next_attempt_at,updated_at,id) WHERE state='queued';
		CREATE INDEX log_investigation_evidence_lookup ON log_investigation_records(record_id);
		CREATE TABLE log_archive_policies (
			organization_id uuid PRIMARY KEY REFERENCES organizations(id) ON DELETE CASCADE,
			keep_days integer NOT NULL CHECK(keep_days=0 OR keep_days BETWEEN 7 AND 3650)
		);
		INSERT INTO dbsphere_schema_migrations(version) VALUES (56)`)
	return err
}

func (s *Postgres) logBatchSummary(ctx context.Context, id string) (logBatchSummary, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return logBatchSummary{}, err
	}
	defer tx.Rollback(ctx)
	value, err := loadLogBatchRollup(ctx, tx, id, false)
	if err != nil {
		return value, err
	}
	return value, tx.Commit(ctx)
}

func loadLogBatchRollup(ctx context.Context, tx pgx.Tx, id string, force bool) (logBatchSummary, error) {
	var value logBatchSummary
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT b.archive_revision`+logBatchScope+
		`WHERE b.id=$1 AND ($2='' OR p.organization_id::text=$2) FOR NO KEY UPDATE OF b`,
		id, OrganizationFromContext(ctx)).Scan(&revision); err != nil {
		return value, logWriteError(err)
	}
	var fresh bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM log_batch_rollups WHERE batch_id=$1 AND revision=$2)`,
		id, revision).Scan(&fresh); err != nil {
		return value, err
	}
	if !fresh || force {
		if err := updateLogBatchRollup(ctx, tx, id, revision); err != nil {
			return value, err
		}
	}
	err := tx.QueryRow(ctx, `SELECT b.id::text,b.name,i.id::text,i.name,p.name,b.coverage,b.coverage_note,
		c.first_at,c.last_at,c.records,c.scanned,c.statements,c.timed,c.slow,c.errors,c.locks,c.deadlocks`+logBatchScope+
		`JOIN log_batch_rollups c ON c.batch_id=b.id WHERE b.id=$1`, id).Scan(value.destinations()...)
	value.score()
	return value, err
}

func updateLogBatchRollup(ctx context.Context, tx pgx.Tx, id string, revision int64) error {
	if _, err := tx.Exec(ctx, "DELETE FROM log_batch_rollups WHERE batch_id=$1", id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO log_batch_rollups
		SELECT $1,$2,`+logBatchCounts+` FROM log_records r WHERE r.batch_id=$1`, id, revision); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO log_issue_rollups
		WITH marked AS (
			SELECT r.id,r.batch_id,r.kind,r.database_name,r.application,r.sql_shape,r.object_key,r.sqlstate,
				r.duration_ms,r.observed_at,`+logIssueGrouping+` AS issue
			FROM log_records r WHERE batch_id=$1 AND observed_at IS NOT NULL
		), issues AS (
			SELECT batch_id,kind,issue,database_name,application,
				coalesce(nullif(min(sql_shape),''),nullif(min(object_key),''),nullif(min(sqlstate),''),kind) label,
				count(*) calls,count(duration_ms) timed,count(*) FILTER (WHERE duration_ms>=1000) slow,
				avg(duration_ms) mean,percentile_cont(0.95) WITHIN GROUP(ORDER BY duration_ms) p95,sum(duration_ms) total
			FROM marked WHERE issue<>'' GROUP BY batch_id,kind,issue,database_name,application
		), anchors AS (
			SELECT DISTINCT ON(kind,issue,database_name,application)
				kind,issue,database_name,application,id
			FROM marked WHERE issue<>''
			ORDER BY kind,issue,database_name,application,duration_ms DESC NULLS LAST,observed_at DESC,id
		) SELECT i.*,a.id FROM issues i JOIN anchors a USING(kind,issue,database_name,application)`, id)
	return err
}

func (s *Postgres) RebuildLogBatchRollup(ctx context.Context, id string) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	value, err := loadLogBatchRollup(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	return value, tx.Commit(ctx)
}

func (s *Postgres) RefreshNextLogBatchRollup(ctx context.Context) (bool, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT b.id::text FROM log_batches b
		LEFT JOIN log_batch_rollups c ON c.batch_id=b.id
		WHERE c.revision IS DISTINCT FROM b.archive_revision
			AND EXISTS (SELECT 1 FROM log_files f WHERE f.batch_id=b.id AND f.state IN ('complete','partial'))
			AND NOT EXISTS (SELECT 1 FROM log_files f WHERE f.batch_id=b.id AND f.state='queued')
		ORDER BY b.created_at DESC,b.id LIMIT 1`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = s.logBatchSummary(ctx, id)
	return err == nil, err
}
