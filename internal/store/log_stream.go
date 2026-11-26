package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

var ErrLogStreamCredential = errors.New("invalid log source credential")

func migrateLogStreams(ctx context.Context, tx pgx.Tx) error {
	var applied bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM dbsphere_schema_migrations WHERE version=65)`).Scan(&applied); err != nil || applied {
		return err
	}
	_, err := tx.Exec(ctx, `CREATE TABLE log_streams (
		id uuid PRIMARY KEY DEFAULT gen_random_uuid(), batch_id uuid NOT NULL REFERENCES log_batches(id) ON DELETE CASCADE,
		name text NOT NULL, format text NOT NULL, prefix text NOT NULL, zone text NOT NULL,
		credential_hash bytea NOT NULL CHECK(octet_length(credential_hash)=32), revoked_at timestamptz,
		generation bigint NOT NULL DEFAULT 1, next_offset bigint NOT NULL DEFAULT 0, filename text NOT NULL DEFAULT '',
		final boolean NOT NULL DEFAULT false, sender_size bigint NOT NULL DEFAULT 0, gap_offset bigint,
		last_received_at timestamptz, UNIQUE(batch_id,name)
	); CREATE TABLE log_stream_receipts (
		stream_id uuid NOT NULL REFERENCES log_streams(id) ON DELETE CASCADE,
		generation bigint NOT NULL, start_offset bigint NOT NULL, raw_size integer NOT NULL,
		filename text NOT NULL, final boolean NOT NULL, digest text NOT NULL, file_id uuid REFERENCES log_files(id) ON DELETE SET NULL,
		PRIMARY KEY(stream_id,generation,start_offset)
	); CREATE INDEX log_stream_receipts_file ON log_stream_receipts(file_id) WHERE file_id IS NOT NULL;
	INSERT INTO dbsphere_schema_migrations(version) VALUES (65)`)
	return err
}

type LogStreamInput struct{ Name, Format, Prefix, Zone string }

type LogStreamChunk struct {
	Generation int64
	Offset     int64
	SenderSize int64
	Filename   string
	Final      bool
	Raw        []byte
}

type LogStreamCursor struct {
	Generation int64  `json:"generation"`
	NextOffset int64  `json:"nextOffset"`
	Filename   string `json:"filename"`
	Final      bool   `json:"final"`
	Duplicate  bool   `json:"duplicate,omitempty"`
	Format     string `json:"format,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
	Zone       string `json:"zone,omitempty"`
}

func logStreamDigest(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

func (s *Postgres) CreateLogStream(ctx context.Context, batch string, input LogStreamInput) (any, error) {
	if !analysisLabel(input.Name, 120) {
		return nil, ErrLogInput
	}
	var engine string
	if err := s.pool.QueryRow(ctx, `SELECT i.engine`+logBatchScope+`WHERE b.id=$1 AND ($2='' OR p.organization_id::text=$2)`,
		batch, OrganizationFromContext(ctx)).Scan(&engine); err != nil {
		return nil, logWriteError(err)
	}
	if engine != "postgres" && engine != "mysql" {
		return nil, ErrLogInput
	}
	if input.Format == "" && engine == "mysql" {
		input.Format = "mysqlslow"
	}
	options, err := logOptionsForEngine(input.Format, input.Prefix, input.Zone, engine)
	if err != nil || options.Format == "auto" || options.Format == "dmtrace" {
		return nil, ErrLogInput
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	token := hex.EncodeToString(random)
	var id string
	err = s.pool.QueryRow(ctx, `INSERT INTO log_streams(batch_id,name,format,prefix,zone,credential_hash)
		SELECT b.id,$3,$4,$5,$6,$7`+logBatchScope+` WHERE b.id=$1 AND ($2='' OR p.organization_id::text=$2)
		RETURNING id::text`, batch, OrganizationFromContext(ctx), strings.TrimSpace(input.Name), options.Format,
		options.Prefix, options.Zone.String(), logStreamDigest(token)).Scan(&id)
	if err != nil {
		return nil, logWriteError(err)
	}
	return map[string]string{"id": id, "token": token}, nil
}

func (s *Postgres) LogStreams(ctx context.Context, batch string) (any, error) {
	rows, err := s.queryJSON(ctx, `SELECT jsonb_build_object('id',stream.id,'name',stream.name,'format',stream.format,
		'generation',stream.generation,'filename',stream.filename,'nextOffset',stream.next_offset,'final',stream.final,
		'senderSize',stream.sender_size,'gapOffset',stream.gap_offset,'lastReceivedAt',stream.last_received_at,
		'revokedAt',stream.revoked_at,'pendingFiles',agg.pending,'failedFiles',agg.failed,'lastParsedAt',agg.parsed)
		FROM log_streams stream JOIN log_batches b ON b.id=stream.batch_id JOIN log_instances i ON i.id=b.instance_id
		JOIN log_projects p ON p.id=i.project_id LEFT JOIN LATERAL (
			SELECT count(*) FILTER (WHERE f.state='queued') AS pending,
			count(*) FILTER (WHERE f.state IN ('partial','failed')) AS failed,
			max(f.updated_at) FILTER (WHERE f.state IN ('partial','complete','failed')) AS parsed
			FROM log_stream_receipts receipt JOIN log_files f ON f.id=receipt.file_id WHERE receipt.stream_id=stream.id
		) agg ON true WHERE stream.batch_id=$1 AND ($2='' OR p.organization_id::text=$2)
		ORDER BY stream.name,stream.id LIMIT 100`, batch, OrganizationFromContext(ctx))
	return map[string]any{"sources": rows}, err
}

func (s *Postgres) RevokeLogStream(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE log_streams SET revoked_at=coalesce(revoked_at,now())
		WHERE id=$1 AND batch_id IN (SELECT b.id`+logBatchScope+` WHERE ($2='' OR p.organization_id::text=$2))`,
		id, OrganizationFromContext(ctx))
	if err == nil && tag.RowsAffected() == 0 {
		return ErrLogNotFound
	}
	return err
}

func (s *Postgres) LogStreamCursor(ctx context.Context, id, token string) (LogStreamCursor, error) {
	var cursor LogStreamCursor
	err := s.pool.QueryRow(ctx, `SELECT generation,next_offset,filename,final,format,prefix,zone FROM log_streams
		WHERE id=$1 AND credential_hash=$2 AND revoked_at IS NULL`, id, logStreamDigest(token)).
		Scan(&cursor.Generation, &cursor.NextOffset, &cursor.Filename, &cursor.Final, &cursor.Format, &cursor.Prefix, &cursor.Zone)
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrLogStreamCredential
	}
	return cursor, err
}

func (s *Postgres) AppendLogStream(ctx context.Context, id, token string, chunk LogStreamChunk) (LogStreamCursor, error) {
	var cursor LogStreamCursor
	size := int64(len(chunk.Raw))
	if chunk.Generation < 1 || chunk.Offset < 0 || size == 0 && !chunk.Final || size > LogArchiveBlockBytes ||
		chunk.Offset > LogFileLimit*1024-size || chunk.SenderSize < chunk.Offset+size ||
		chunk.SenderSize > LogFileLimit*1024 ||
		!analysisLabel(chunk.Filename, 180) || strings.ContainsAny(chunk.Filename, "/\\") ||
		!chunk.Final && chunk.Raw[len(chunk.Raw)-1] != '\n' ||
		chunk.Final && chunk.SenderSize != chunk.Offset+size {
		return cursor, ErrLogInput
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return cursor, err
	}
	defer tx.Rollback(ctx)
	var batch, org, format, prefix, zone string
	err = tx.QueryRow(ctx, `SELECT stream.batch_id::text,p.organization_id::text,stream.generation,stream.next_offset,
		stream.filename,stream.final,stream.format,stream.prefix,stream.zone
		FROM log_streams stream JOIN log_batches b ON b.id=stream.batch_id JOIN log_instances i ON i.id=b.instance_id
		JOIN log_projects p ON p.id=i.project_id WHERE stream.id=$1 AND stream.credential_hash=$2 AND stream.revoked_at IS NULL
		FOR UPDATE OF stream`, id, logStreamDigest(token)).Scan(&batch, &org, &cursor.Generation, &cursor.NextOffset,
		&cursor.Filename, &cursor.Final, &format, &prefix, &zone)
	if errors.Is(err, pgx.ErrNoRows) {
		return cursor, ErrLogStreamCredential
	}
	if err != nil {
		return cursor, err
	}
	sum := sha256.Sum256(chunk.Raw)
	digest := hex.EncodeToString(sum[:])
	if chunk.Generation < cursor.Generation || chunk.Generation == cursor.Generation && (chunk.Offset < cursor.NextOffset || cursor.Final) {
		var previous, filename string
		var final bool
		err := tx.QueryRow(ctx, `SELECT digest,filename,final FROM log_stream_receipts
			WHERE stream_id=$1 AND generation=$2 AND start_offset=$3`, id, chunk.Generation, chunk.Offset).Scan(&previous, &filename, &final)
		if err == nil && previous == digest && filename == chunk.Filename && final == chunk.Final {
			cursor.Duplicate = true
			return cursor, nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return cursor, err
		}
		return cursor, ErrLogConflict
	}
	rotated := cursor.Final && cursor.Generation < 1<<63-1 && chunk.Generation == cursor.Generation+1 && chunk.Offset == 0
	if !rotated && (chunk.Generation != cursor.Generation || cursor.Final || chunk.Offset != cursor.NextOffset ||
		cursor.Filename != "" && cursor.Filename != chunk.Filename) {
		if chunk.Generation == cursor.Generation && !cursor.Final && chunk.Offset > cursor.NextOffset &&
			(cursor.Filename == "" || cursor.Filename == chunk.Filename) {
			if _, err := tx.Exec(ctx, `UPDATE log_streams SET gap_offset=$2,sender_size=greatest(sender_size,$3) WHERE id=$1`,
				id, chunk.Offset, chunk.SenderSize); err != nil {
				return cursor, err
			}
			if err := tx.Commit(ctx); err != nil {
				return cursor, err
			}
		}
		return cursor, ErrLogConflict
	}
	if size == 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO log_stream_receipts(stream_id,generation,start_offset,raw_size,filename,final,digest)
			VALUES($1,$2,$3,0,$4,true,$5)`, id, chunk.Generation, chunk.Offset, chunk.Filename, digest); err != nil {
			return cursor, err
		}
	} else if err := saveLogStreamChunk(ctx, tx, id, batch, org, format, prefix, zone, digest, chunk); err != nil {
		return cursor, err
	}
	next := chunk.Offset + size
	if _, err := tx.Exec(ctx, `UPDATE log_streams SET generation=$2,next_offset=$3,filename=$4,final=$5,
		sender_size=CASE WHEN generation<>$2 THEN $6 ELSE greatest(sender_size,$6) END,
		gap_offset=CASE WHEN generation=$2 AND gap_offset>$3 THEN gap_offset END,last_received_at=now() WHERE id=$1`,
		id, chunk.Generation, next, chunk.Filename, chunk.Final, chunk.SenderSize); err != nil {
		return cursor, err
	}
	if err := tx.Commit(ctx); err != nil {
		return cursor, fmt.Errorf("commit log stream chunk: %w", err)
	}
	return LogStreamCursor{Generation: chunk.Generation, NextOffset: next, Filename: chunk.Filename, Final: chunk.Final}, nil
}

func saveLogStreamChunk(ctx context.Context, tx pgx.Tx, id, batch, org, format, prefix, zone, digest string, chunk LogStreamChunk) error {
	size := int64(len(chunk.Raw))
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,56))`, org); err != nil {
		return err
	}
	var outstanding, reserved int64
	if err := tx.QueryRow(ctx, `SELECT count(*),coalesce(sum(f.expected_size),0)`+logFileScope+
		`WHERE p.organization_id=$1 AND f.state IN ('uploading','queued')`, org).Scan(&outstanding, &reserved); err != nil {
		return err
	}
	if outstanding >= logUploadLimit || reserved+size > logOutstandingLimit {
		return ErrLogQuota
	}
	encoder, err := logEncoder()
	if err != nil {
		return err
	}
	archive := encoder.EncodeAll(chunk.Raw, nil)
	var file string
	err = tx.QueryRow(ctx, `INSERT INTO log_files(batch_id,filename,node,expected_size,format,prefix,zone,
		state,raw_size,stored_size,parts) VALUES($1,$2,$3,$4,$5,$6,$7,'queued',$4,$8,1) RETURNING id::text`,
		batch, chunk.Filename, "stream:"+id, size, format, prefix, zone, len(archive)).Scan(&file)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO log_archive_blocks(file_id,part,raw_size,digest,compressed)
		VALUES($1,0,$2,$3,$4)`, file, size, digest, archive); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO log_stream_receipts(stream_id,generation,start_offset,raw_size,filename,final,digest,file_id)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, chunk.Generation, chunk.Offset, size, chunk.Filename, chunk.Final, digest, file); err != nil {
		return err
	}
	return nil
}

