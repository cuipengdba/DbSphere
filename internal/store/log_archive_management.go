package store

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/jackc/pgx/v5"
)

const logUploadLimit = 64
const logOutstandingLimit = 32 << 30

var ErrLogQuota = errors.New("log upload quota exceeded")

func (s *Postgres) LogUploadStatus(ctx context.Context, file string) (any, error) {
	var value json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT jsonb_build_object('id',f.id,'batchId',b.id,'state',f.state,
		'parts',f.parts,'rawSize',f.raw_size,'chunkBytes',$3::int,'size',f.expected_size,
		'digests',coalesce((SELECT jsonb_agg(digest ORDER BY part) FROM log_archive_blocks WHERE file_id=f.id),'[]'::jsonb))`+
		logFileScope+`WHERE f.id=$1 AND ($2='' OR p.organization_id::text=$2)`,
		file, OrganizationFromContext(ctx), LogArchiveBlockBytes).Scan(&value)
	return value, logWriteError(err)
}

func (s *Postgres) LogArchiveOverview(ctx context.Context) (any, error) {
	source, err := s.queryJSON(ctx, `SELECT jsonb_build_object('organizationId',o.id,'name',o.name,
		'keepDays',coalesce(policy.keep_days,0),'files',count(f.id),
		'rawSize',coalesce(sum(f.raw_size) FILTER(WHERE f.stored_size>0),0),'storedSize',coalesce(sum(f.stored_size),0),
		'queued',count(f.id) FILTER(WHERE f.state='queued'),'uploading',count(f.id) FILTER(WHERE f.state='uploading'),
		'expired',count(f.id) FILTER(WHERE f.archive_expired_at IS NOT NULL))
		FROM organizations o LEFT JOIN log_archive_policies policy ON policy.organization_id=o.id
		LEFT JOIN log_projects p ON p.organization_id=o.id LEFT JOIN log_instances i ON i.project_id=p.id
		LEFT JOIN log_batches b ON b.instance_id=i.id LEFT JOIN log_files f ON f.batch_id=b.id
		WHERE ($1='' OR o.id::text=$1) GROUP BY o.id,policy.keep_days ORDER BY o.name,o.id`, OrganizationFromContext(ctx))
	return map[string]any{"organizations": source, "uploadLimit": logUploadLimit, "pendingBytesLimit": int64(logOutstandingLimit)}, err
}

func (s *Postgres) SetLogArchivePolicy(ctx context.Context, organization string, keepDays int) (any, error) {
	if keepDays != 0 && (keepDays < 7 || keepDays > 3650) {
		return nil, ErrLogInput
	}
	updated, err := s.pool.Exec(ctx, `INSERT INTO log_archive_policies(organization_id,keep_days)
		SELECT id,$2 FROM organizations WHERE id=$1 AND ($3='' OR id::text=$3)
		ON CONFLICT(organization_id) DO UPDATE SET keep_days=excluded.keep_days`,
		organization, keepDays, OrganizationFromContext(ctx))
	if err != nil {
		return nil, err
	}
	if updated.RowsAffected() != 1 {
		return nil, ErrLogNotFound
	}
	return s.LogArchiveOverview(ctx)
}

// SweepLogArchives removes one expired raw file or abandoned upload at a time.
func (s *Postgres) SweepLogArchives(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var file string
	err = tx.QueryRow(ctx, `SELECT f.id::text`+logFileScope+
		`LEFT JOIN log_archive_policies a ON a.organization_id=p.organization_id
		WHERE (f.state='uploading' AND f.updated_at<now()-interval '7 days')
		OR (a.keep_days>0 AND f.state IN ('complete','partial','failed') AND f.stored_size>0
			AND f.updated_at<now()-make_interval(days=>a.keep_days)
			AND NOT EXISTS (SELECT 1 FROM log_records r
				JOIN log_investigation_records q ON q.record_id=r.id WHERE r.file_id=f.id))
		ORDER BY f.updated_at,f.id FOR UPDATE OF f SKIP LOCKED LIMIT 1`).Scan(&file)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	tag, err := tx.Exec(ctx, "DELETE FROM log_files WHERE id=$1 AND state='uploading'", file)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		var pinned bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM log_records r JOIN log_investigation_records q
			ON q.record_id=r.id WHERE r.file_id=$1)`, file).Scan(&pinned); err != nil || pinned {
			return false, err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM log_archive_blocks WHERE file_id=$1", file); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, "UPDATE log_files SET stored_size=0,archive_expired_at=now() WHERE id=$1", file); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

func (s *Postgres) VerifyLogFile(ctx context.Context, file string) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var parts int
	var rawSize int64
	err = tx.QueryRow(ctx, `SELECT f.parts,f.raw_size`+logFileScope+
		`WHERE f.id=$1 AND f.state IN ('complete','partial','failed') AND f.stored_size>0
			AND ($2='' OR p.organization_id::text=$2) FOR NO KEY UPDATE OF f`,
		file, OrganizationFromContext(ctx)).Scan(&parts, &rawSize)
	if err != nil {
		return nil, logWriteError(err)
	}
	reader := &logArchiveReader{ctx: ctx, db: tx, file: file, parts: parts}
	checked, err := io.Copy(io.Discard, reader)
	if err != nil {
		return nil, err
	}
	if checked != rawSize {
		return nil, ErrLogConflict
	}
	if _, err := tx.Exec(ctx, "UPDATE log_files SET verified_at=now() WHERE id=$1", file); err != nil {
		return nil, err
	}
	return map[string]any{"id": file, "verified": true, "rawSize": checked, "parts": parts}, tx.Commit(ctx)
}
