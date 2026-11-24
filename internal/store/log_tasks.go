package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

func migrateLogTasks(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=57)").Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE INDEX log_files_tasks_recent ON log_files(created_at DESC,id DESC);
		INSERT INTO dbsphere_schema_migrations(version) VALUES (57)`)
	return err
}

type LogTaskQuery struct {
	State    string
	Before   *time.Time
	BeforeID string
}

func (s *Postgres) LogTasks(ctx context.Context, query LogTaskQuery) (any, error) {
	switch query.State {
	case "", "uploading", "queued", "failed", "partial", "complete", "duplicate":
	default:
		return nil, ErrLogInput
	}
	const limit = 40
	org := OrganizationFromContext(ctx)
	rows, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',f.id,'batchId',b.id,'project',p.name,
		'instance',i.name,'environment',i.environment,'batch',b.name,'filename',f.filename,'state',f.state,
		'node',f.node,'receivedBytes',f.raw_size,'expectedBytes',f.expected_size,'archiveBytes',f.stored_size,
		'attempts',f.processing_attempts,'nextAttemptAt',f.next_attempt_at,'reason',f.reason,
		'createdAt',f.created_at,'updatedAt',f.updated_at)`+logFileScope+`
		WHERE ($1='' OR p.organization_id::text=$1) AND ($2='' OR f.state=$2)
			AND ($3::timestamptz IS NULL OR (f.created_at,f.id)<($3,nullif($4,'')::uuid))
		ORDER BY f.created_at DESC,f.id DESC LIMIT $5`, org, query.State, query.Before, query.BeforeID, limit+1)
	if err != nil {
		return nil, err
	}
	var next any
	if len(rows) > limit {
		var cursor struct {
			ID        string    `json:"id"`
			CreatedAt time.Time `json:"createdAt"`
		}
		if err := json.Unmarshal(rows[limit-1], &cursor); err != nil {
			return nil, err
		}
		next = cursor
		rows = rows[:limit]
	}
	var summary json.RawMessage
	err = s.pool.QueryRow(ctx, `SELECT jsonb_build_object(
		'uploading',count(*) FILTER(WHERE f.state='uploading'),
		'queued',count(*) FILTER(WHERE f.state='queued'),
		'reservedBytes',coalesce(sum(f.expected_size),0),
		'oldestQueuedAt',min(f.updated_at) FILTER(WHERE f.state='queued'))`+logFileScope+`
		WHERE ($1='' OR p.organization_id::text=$1) AND f.state IN ('uploading','queued')`, org).Scan(&summary)
	return map[string]any{"tasks": rows, "summary": summary, "next": next, "organizationScoped": org != "",
		"fileLimit": logUploadLimit, "byteLimit": int64(logOutstandingLimit)}, err
}

func (s *Postgres) CancelLogUpload(ctx context.Context, file string) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	org := OrganizationFromContext(ctx)
	var id string
	err = tx.QueryRow(ctx, `SELECT f.id::text`+logFileScope+`
		WHERE f.id=$1 AND ($2='' OR p.organization_id::text=$2) AND f.state='uploading' FOR UPDATE OF f`,
		file, org).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1`+logFileScope+`
			WHERE f.id=$1 AND ($2='' OR p.organization_id::text=$2))`, file, org).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			return nil, ErrLogConflict
		}
	}
	if err != nil {
		return nil, logWriteError(err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM log_files WHERE id=$1", file); err != nil {
		return nil, err
	}
	return map[string]string{"id": file, "state": "cancelled"}, tx.Commit(ctx)
}
