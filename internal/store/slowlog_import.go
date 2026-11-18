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

type slowLogEntry struct {
	SourceKey    string
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
	var timestamp, dbname, message, rawQueryID string
	if format == "csvlog" {
		if len(fields) < 14 {
			return slowLogEntry{}, false, nil
		}
		timestamp, dbname, message = fields[0], fields[2], fields[13]
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
	if slowQueryID.MatchString(rawQueryID) && rawQueryID != "0" {
		entry.QueryID = &rawQueryID
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
	var database string
	if err := s.pool.QueryRow(ctx, `SELECT database_name FROM assets WHERE id=$1`, assetID).
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	imported := 0
	for _, entry := range entries {
		result, err := tx.Exec(ctx, `INSERT INTO slow_log_entries
			(id,asset_id,source_key,observed_at,duration_ms,query_id,source_format,imported_at)
			VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,$6,now())
			ON CONFLICT(asset_id,source_key) DO NOTHING`, assetID, entry.SourceKey,
			entry.ObservedAt, entry.DurationMs, entry.QueryID, entry.SourceFormat)
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
