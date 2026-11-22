package store

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrInvalidSlowLog = errors.New("invalid slow log import")
var slowDuration = regexp.MustCompile(`^duration: ([0-9]+(?:\.[0-9]+)?) ms(?:\s|$)`)
var slowQueryID = regexp.MustCompile(`^-?\d{1,20}$`)
var pgLogSessionID = regexp.MustCompile(`(?i)^[0-9a-f]{1,16}\.[0-9a-f]{1,16}$`)

type slowLogEntry struct {
	SourceKey    string
	RecordKey    *string
	ObservedAt   time.Time
	DurationMs   float64
	QueryID      *string
	SourceFormat string
}

func parseSlowLogTime(raw string) (time.Time, bool) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.000 MST",
		"2006-01-02 15:04:05 MST", "2006-01-02 15:04:05.000 -0700",
		"2006-01-02 15:04:05 -0700"} {
		if at, err := time.Parse(layout, raw); err == nil {
			return at.UTC(), true
		}
	}
	return time.Time{}, false
}

func parseSlowLogRecord(format, raw, database string, fields []string) (slowLogEntry, bool, error) {
	var timestamp, dbname, message, rawQueryID, sessionID, lineNumber string
	if format == "csvlog" {
		if len(fields) < 14 {
			return slowLogEntry{}, false, nil
		}
		timestamp, dbname, message = fields[0], fields[2], fields[13]
		sessionID, lineNumber = fields[5], fields[6]
		if len(fields) > 25 {
			rawQueryID = fields[25]
		}
	} else {
		var record map[string]json.RawMessage
		if json.Unmarshal([]byte(raw), &record) != nil {
			return slowLogEntry{}, false, ErrInvalidSlowLog
		}
		_ = json.Unmarshal(record["timestamp"], &timestamp)
		_ = json.Unmarshal(record["dbname"], &dbname)
		_ = json.Unmarshal(record["message"], &message)
		_ = json.Unmarshal(record["session_id"], &sessionID)
		lineNumber = strings.Trim(string(record["line_num"]), `"`)
		if value := record["query_id"]; len(value) > 0 {
			rawQueryID = strings.Trim(string(value), `"`)
		}
	}
	if dbname != database {
		return slowLogEntry{}, false, nil
	}
	match := slowDuration.FindStringSubmatch(message)
	if len(match) != 2 {
		return slowLogEntry{}, false, nil
	}
	elapsed, err := strconv.ParseFloat(match[1], 64)
	if err != nil || elapsed < 0 || elapsed > 1e9 {
		return slowLogEntry{}, false, nil
	}
	at, ok := parseSlowLogTime(timestamp)
	if !ok {
		return slowLogEntry{}, false, nil
	}
	hash := sha256.Sum256([]byte(raw))
	entry := slowLogEntry{SourceKey: hex.EncodeToString(hash[:]), ObservedAt: at,
		DurationMs: elapsed, SourceFormat: format}
	if queryID := canonicalSQLQueryID(rawQueryID); queryID != "" {
		entry.QueryID = &queryID
	}
	if line, err := strconv.ParseInt(lineNumber, 10, 64); err == nil && line > 0 && pgLogSessionID.MatchString(sessionID) {
		// PostgreSQL identifies each emitted CSV/JSON record by session and line.
		// Timestamp limits accidental collisions when archives from rebuilt servers are combined.
		identity := database + "\x00" + strings.ToLower(sessionID) + "\x00" + strconv.FormatInt(line, 10) + "\x00" + at.Truncate(time.Millisecond).Format(time.RFC3339Nano)
		digest := sha256.Sum256([]byte(identity))
		value := hex.EncodeToString(digest[:])
		entry.RecordKey = &value
	}
	return entry, true, nil
}

func parseSlowLogs(format, content, database string) ([]slowLogEntry, int, error) {
	if (format != "jsonlog" && format != "csvlog") || len(content) > 60000 || database == "" {
		return nil, 0, ErrInvalidSlowLog
	}
	entries := []slowLogEntry{}
	scanned := 0
	if format == "csvlog" {
		reader := csv.NewReader(strings.NewReader(content))
		reader.FieldsPerRecord = -1
		start := int64(0)
		for {
			fields, err := reader.Read()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, 0, ErrInvalidSlowLog
			}
			end := reader.InputOffset()
			raw := content[start:end]
			start = end
			if len(fields) == 1 && fields[0] == "" {
				continue
			}
			scanned++
			if scanned > 200 {
				return nil, 0, ErrInvalidSlowLog
			}
			entry, keep, err := parseSlowLogRecord(format, raw, database, fields)
			if err != nil {
				return nil, 0, err
			}
			if keep {
				entries = append(entries, entry)
			}
		}
	} else {
		for _, line := range strings.Split(content, "\n") {
			raw := strings.TrimSuffix(line, "\r")
			if raw == "" {
				continue
			}
			scanned++
			if scanned > 200 {
				return nil, 0, ErrInvalidSlowLog
			}
			entry, keep, err := parseSlowLogRecord(format, raw, database, nil)
			if err != nil {
				return nil, 0, err
			}
			if keep {
				entries = append(entries, entry)
			}
		}
	}
	return entries, scanned, nil
}

func (s *Postgres) ImportSlowLogs(ctx context.Context, assetID, format, content string) (any, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var database string
	if err := tx.QueryRow(ctx, `SELECT database_name FROM assets WHERE id=$1 FOR NO KEY UPDATE`, assetID).
		Scan(&database); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAssetNotFound
		}
		return nil, err
	}
	entries, scanned, err := parseSlowLogs(format, content, database)
	if err != nil {
		return nil, err
	}
	imported := 0
	for _, entry := range entries {
		if entry.RecordKey != nil {
			if _, err := tx.Exec(ctx, `UPDATE slow_log_entries SET database_name=$3,record_key=$4
				WHERE asset_id=$1 AND source_key=$2 AND record_key IS NULL AND (database_name IS NULL OR database_name=$3)
				AND NOT EXISTS (SELECT 1 FROM slow_log_entries WHERE asset_id=$1 AND record_key=$4)`,
				assetID, entry.SourceKey, database, entry.RecordKey); err != nil {
				return nil, err
			}
		}
		result, err := tx.Exec(ctx, `INSERT INTO slow_log_entries
			(id,asset_id,source_key,observed_at,duration_ms,query_id,source_format,database_name,record_key,imported_at)
			VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,$6,$7,$8,now())
			ON CONFLICT DO NOTHING`, assetID, entry.SourceKey, entry.ObservedAt, entry.DurationMs,
			entry.QueryID, entry.SourceFormat, database, entry.RecordKey)
		if err != nil {
			return nil, err
		}
		imported += int(result.RowsAffected())
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{"scanned": scanned, "ignored": scanned - len(entries),
		"imported": imported, "duplicates": len(entries) - imported}, nil
}
