package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"github.com/cuipengdba/dbsphere/internal/loganalysis"
	"github.com/jackc/pgx/v5"
	"github.com/klauspost/compress/zstd"
)

const LogArchiveBlockBytes = 1 << 20
const LogFileLimit = 512 << 20

var logEncoder = sync.OnceValues(func() (*zstd.Encoder, error) {
	return zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
})

var logDecoder = sync.OnceValues(func() (*zstd.Decoder, error) {
	return zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(8<<20), zstd.WithDecodeAllCapLimit(true))
})

type LogFileInput struct {
	Name, Node, Format, Prefix, Zone string
	Size                             int64
}

type LogParseInput struct{ Format, Prefix, Zone string }

func logOptions(format, prefix, zone string) (loganalysis.Options, error) {
	if format == "" {
		format = "auto"
	}
	if zone == "" {
		zone = "UTC"
	}
	if len(zone) > 64 || strings.ContainsAny(zone, "\x00\r\n") {
		return loganalysis.Options{}, ErrLogInput
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return loganalysis.Options{}, ErrLogInput
	}
	options := loganalysis.Options{Format: format, Prefix: prefix, Zone: location}
	if loganalysis.ValidateOptions(options) != nil {
		return loganalysis.Options{}, ErrLogInput
	}
	return options, nil
}

func (s *Postgres) CreateLogFile(ctx context.Context, batch string, input LogFileInput) (any, error) {
	options, err := logOptions(input.Format, input.Prefix, input.Zone)
	if err != nil || !analysisLabel(input.Name, 255) || strings.ContainsAny(input.Name, "/\\") ||
		len(input.Node) > 120 || strings.ContainsAny(input.Node, "\x00\r\n") || input.Size <= 0 || input.Size > LogFileLimit {
		return nil, ErrLogInput
	}
	var value json.RawMessage
	err = s.pool.QueryRow(ctx, `INSERT INTO log_files(batch_id,filename,node,expected_size,format,prefix,zone)
		SELECT b.id,$3,$4,$5,$6,$7,$8`+logBatchScope+` WHERE b.id=$1 AND ($2='' OR p.organization_id::text=$2)
		RETURNING jsonb_build_object('id',id,'state',state,'parts',parts,'rawSize',raw_size,'chunkBytes',$9::int)`,
		batch, OrganizationFromContext(ctx), input.Name, strings.TrimSpace(input.Node), input.Size,
		options.Format, options.Prefix, options.Zone.String(), LogArchiveBlockBytes).Scan(&value)
	return value, logWriteError(err)
}

func (s *Postgres) AppendLogBlock(ctx context.Context, file string, part int, raw []byte) (any, error) {
	if part < 0 || len(raw) == 0 || len(raw) > LogArchiveBlockBytes {
		return nil, ErrLogInput
	}
	sha := sha256.Sum256(raw)
	digest := hex.EncodeToString(sha[:])
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var parts int
	var received, expected int64
	var state string
	err = tx.QueryRow(ctx, `SELECT f.parts,f.raw_size,f.expected_size,f.state`+logFileScope+
		`WHERE f.id=$1 AND ($2='' OR p.organization_id::text=$2) FOR UPDATE OF f`,
		file, OrganizationFromContext(ctx)).Scan(&parts, &received, &expected, &state)
	if err != nil {
		return nil, logWriteError(err)
	}
	if part < parts {
		var existing string
		if err := tx.QueryRow(ctx, "SELECT digest FROM log_archive_blocks WHERE file_id=$1 AND part=$2", file, part).Scan(&existing); err != nil {
			return nil, logWriteError(err)
		}
		if existing != digest {
			return nil, ErrLogConflict
		}
		return map[string]any{"parts": parts, "rawSize": received}, nil
	}
	if state != "uploading" || part != parts || int64(len(raw)) != min(LogArchiveBlockBytes, expected-received) {
		return nil, ErrLogConflict
	}
	encoder, err := logEncoder()
	if err != nil {
		return nil, err
	}
	archive := encoder.EncodeAll(raw, nil)
	if _, err := tx.Exec(ctx, `INSERT INTO log_archive_blocks(file_id,part,raw_size,digest,compressed)
		VALUES($1,$2,$3,$4,$5)`, file, part, len(raw), digest, archive); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE log_files SET parts=parts+1,raw_size=raw_size+$2,stored_size=stored_size+$3,updated_at=now()
		WHERE id=$1`, file, len(raw), len(archive)); err != nil {
		return nil, err
	}
	return map[string]any{"parts": parts + 1, "rawSize": received + int64(len(raw))}, tx.Commit(ctx)
}

func (s *Postgres) QueueLogFile(ctx context.Context, file string, override *LogParseInput) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var state, batch, instance, node, format, prefix, zone string
	var received, expected int64
	err = tx.QueryRow(ctx, `SELECT f.state,b.id::text,i.id::text,f.node,f.format,f.prefix,f.zone,f.raw_size,f.expected_size`+
		logFileScope+`WHERE f.id=$1 AND ($2='' OR p.organization_id::text=$2) FOR UPDATE OF f`,
		file, OrganizationFromContext(ctx)).Scan(&state, &batch, &instance, &node, &format, &prefix, &zone, &received, &expected)
	if err != nil {
		return nil, logWriteError(err)
	}
	if received != expected {
		return nil, ErrLogConflict
	}
	if state == "failed" {
		if override == nil {
			return map[string]any{"state": state, "batchId": batch}, nil
		}
		opts, err := logOptions(override.Format, override.Prefix, override.Zone)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE log_files SET state='queued',format=$2,prefix=$3,zone=$4,reason='',updated_at=now()
			WHERE id=$1`, file, opts.Format, opts.Prefix, opts.Zone.String()); err != nil {
			return nil, err
		}
		return map[string]any{"state": "queued", "batchId": batch}, tx.Commit(ctx)
	}
	if state == "duplicate" {
		var original *string
		if err := tx.QueryRow(ctx, `SELECT original.batch_id::text FROM log_files f
			LEFT JOIN log_files original ON original.id=f.duplicate_of WHERE f.id=$1`, file).Scan(&original); err != nil {
			return nil, err
		}
		return map[string]any{"state": state, "batchId": batch, "duplicateBatch": original}, nil
	}
	if state != "uploading" {
		return map[string]any{"state": state, "batchId": batch}, nil
	}
	var key string
	if err := tx.QueryRow(ctx, `SELECT encode(sha256(convert_to(string_agg(digest,'' ORDER BY part),'UTF8')),'hex')
		FROM log_archive_blocks WHERE file_id=$1`, file).Scan(&key); err != nil {
		return nil, err
	}
	tag, err := tx.Exec(ctx, "INSERT INTO log_source_keys(instance_id,node,digest,file_id) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING",
		instance, node, key, file)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		var original, originalBatch string
		err := tx.QueryRow(ctx, `SELECT k.file_id::text,f.batch_id::text FROM log_source_keys k
			JOIN log_files f ON f.id=k.file_id WHERE k.instance_id=$1 AND k.node=$2 AND k.digest=$3`,
			instance, node, key).Scan(&original, &originalBatch)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM log_archive_blocks WHERE file_id=$1", file); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "UPDATE log_files SET state='duplicate',stored_size=0,duplicate_of=$2,updated_at=now() WHERE id=$1", file, original); err != nil {
			return nil, err
		}
		return map[string]any{"state": "duplicate", "batchId": batch, "duplicateBatch": originalBatch}, tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, "UPDATE log_files SET state='queued',updated_at=now() WHERE id=$1", file); err != nil {
		return nil, err
	}
	return map[string]any{"state": "queued", "batchId": batch}, tx.Commit(ctx)
}

type logArchiveReader struct {
	ctx context.Context
	db  interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	}
	file  string
	part  int
	parts int
	block []byte
}

func readLogBlock(compressed []byte, size int, expected string) ([]byte, error) {
	if size <= 0 || size > LogArchiveBlockBytes {
		return nil, errors.New("invalid archive block size")
	}
	decoder, err := logDecoder()
	if err != nil {
		return nil, err
	}
	raw, err := decoder.DecodeAll(compressed, make([]byte, 0, size))
	if err != nil || len(raw) != size {
		return nil, errors.New("invalid compressed log block")
	}
	hash := sha256.Sum256(raw)
	if hex.EncodeToString(hash[:]) != expected {
		return nil, errors.New("log block digest mismatch")
	}
	return raw, nil
}

func (r *logArchiveReader) Read(out []byte) (int, error) {
	if len(out) == 0 {
		return 0, nil
	}
	if len(r.block) == 0 {
		if r.part >= r.parts {
			return 0, io.EOF
		}
		var compressed []byte
		var size int
		var expected string
		err := r.db.QueryRow(r.ctx, "SELECT compressed,raw_size,digest FROM log_archive_blocks WHERE file_id=$1 AND part=$2",
			r.file, r.part).Scan(&compressed, &size, &expected)
		if err != nil {
			return 0, err
		}
		r.block, err = readLogBlock(compressed, size, expected)
		if err != nil {
			return 0, err
		}
		r.part++
	}
	n := copy(out, r.block)
	r.block = r.block[n:]
	return n, nil
}

var logRecordColumns = []string{"batch_id", "file_id", "record_key", "raw_offset", "raw_length", "observed_at",
	"kind", "severity", "database_name", "application", "session_key", "process_id", "blocker_id", "sqlstate",
	"sql_shape", "fingerprint", "command", "duration_ms", "temp_bytes", "object_key", "checkpoint_buffers"}

func (s *Postgres) ProcessNextLogFile(ctx context.Context) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var file, batch, node, engine, format, prefix, zone string
	var parts int
	err = tx.QueryRow(ctx, `SELECT f.id,b.id,f.node,i.engine,f.format,f.prefix,f.zone,f.parts`+logFileScope+
		`WHERE f.state='queued' ORDER BY f.updated_at,f.id FOR UPDATE OF f SKIP LOCKED LIMIT 1`).
		Scan(&file, &batch, &node, &engine, &format, &prefix, &zone, &parts)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	options, err := logOptions(format, prefix, zone)
	if err != nil {
		return false, err
	}
	options.Engine = engine
	if _, err := tx.Exec(ctx, "CREATE TEMPORARY TABLE ingest_log_records (LIKE log_records INCLUDING DEFAULTS) ON COMMIT DROP"); err != nil {
		return false, err
	}
	var parsed, count int
	rows := make([][]any, 0, 256)
	flush := func() error {
		if len(rows) == 0 {
			return nil
		}
		if _, err := tx.CopyFrom(ctx, pgx.Identifier{"ingest_log_records"}, logRecordColumns, pgx.CopyFromRows(rows)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "INSERT INTO log_records SELECT * FROM ingest_log_records ON CONFLICT DO NOTHING"); err != nil {
			return err
		}
		rows = rows[:0]
		_, err := tx.Exec(ctx, "TRUNCATE ingest_log_records")
		return err
	}
	reader := &logArchiveReader{ctx: ctx, db: tx, file: file, parts: parts}
	resolved, err := loganalysis.Parse(reader, options, func(record loganalysis.Record) error {
		count++
		var at *time.Time
		if !record.At.IsZero() {
			at = &record.At
			parsed++
		}
		var identity *string
		if record.Identity != "" {
			key := node + "/" + record.Identity
			identity = &key
		}
		rows = append(rows, []any{batch, file, identity, record.Offset, int64(record.Length), at, record.Kind,
			limitedLogText(record.Severity, 40), limitedLogText(record.Database, 256), limitedLogText(record.Application, 256),
			limitedLogText(record.Session, 128), limitedLogText(record.Process, 40), limitedLogText(record.Blocker, 40),
			limitedLogText(record.SQLState, 40), limitedLogText(record.Shape, 8192), record.Fingerprint,
			limitedLogText(record.Command, 40), record.DurationMS, record.TempBytes,
			limitedLogText(record.ObjectKey, 180), record.CheckpointBuffers})
		if len(rows) == cap(rows) {
			return flush()
		}
		return nil
	})
	state, reason := "complete", ""
	switch {
	case errors.Is(err, loganalysis.ErrFormat):
		state, reason = "failed", "无法识别日志格式。请选择格式和前缀后重新解析。"
	case err != nil:
		return false, fmt.Errorf("read queued log file: %w", err)
	case parsed == 0:
		state, reason = "failed", "未识别到有效时间戳。请检查格式、前缀和时区。"
	case parsed < count:
		state, reason = "partial", "可以在记录列表筛选未识别内容。"
	}
	if state != "failed" {
		if err := flush(); err != nil {
			return false, err
		}
	} else if _, err := tx.Exec(ctx, "DELETE FROM log_records WHERE file_id=$1", file); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE log_files SET state=$2,reason=$3,detected_format=$4,
		record_count=$5,parsed_count=$6,updated_at=now() WHERE id=$1`, file, state, reason, resolved, count, parsed); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func limitedLogText(value string, limit int) string {
	value = strings.ReplaceAll(value, "\x00", "")
	if len(value) > limit {
		value = strings.ToValidUTF8(value[:limit], "")
	}
	return value
}

func (s *Postgres) LogOriginal(ctx context.Context, record string) (any, error) {
	var file, name string
	var offset, size, fileSize int64
	var parts int
	err := s.pool.QueryRow(ctx, `SELECT f.id,f.filename,r.raw_offset,r.raw_length,f.raw_size,f.parts`+logFileScope+
		`JOIN log_records r ON r.file_id=f.id WHERE r.id=$1 AND ($2='' OR p.organization_id::text=$2)`,
		record, OrganizationFromContext(ctx)).Scan(&file, &name, &offset, &size, &fileSize, &parts)
	if err != nil {
		return nil, logWriteError(err)
	}
	// Return the selected record and a bounded amount of adjacent context.
	from := max(0, offset-2048)
	to := min(fileSize, offset+min(size, LogArchiveBlockBytes)+2048)
	first := int(from / LogArchiveBlockBytes)
	reader := &logArchiveReader{ctx: ctx, db: s.pool, file: file, part: first, parts: parts}
	if _, err := io.CopyN(io.Discard, reader, from%LogArchiveBlockBytes); err != nil {
		return nil, err
	}
	raw := make([]byte, to-from)
	if _, err := io.ReadFull(reader, raw); err != nil {
		return nil, err
	}
	return map[string]any{"file": name, "offset": offset, "size": size, "contextOffset": from,
		"truncated": size > LogArchiveBlockBytes, "text": strings.ToValidUTF8(string(raw), "�")}, nil
}
