package loganalysis

import (
	"bufio"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
)

var (
	mysqlSlowHeader   = regexp.MustCompile(`(?m)^# Time: (\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?)[ \t\r]*$`)
	mysqlSlowDuration = regexp.MustCompile(`(?m)^# Query_time:\s*(\d+(?:\.\d+)?)\b`)
	mysqlSlowProcess  = regexp.MustCompile(`(?m)^# User@Host:[^\r\n]*?\bId:\s*(\d+)\s*$`)
	mysqlSlowSchema   = regexp.MustCompile(`(?im)^# (?:Thread_id:\s*\d+\s+)?Schema:\s*([a-zA-Z0-9_$]+)\b`)
	mysqlSlowUse      = regexp.MustCompile("(?i)^use[ \\t]+`?([a-zA-Z0-9_$]+)`?;[ \\t\\r]*$")
	mysqlSlowStart    = regexp.MustCompile(`(?i)^SET timestamp=\d+;[ \t\r]*$`)
)

func parseMySQLSlow(source *bufio.Reader, zone *time.Location, accept func(Record) error) error {
	if zone == nil {
		zone = time.UTC
	}
	var offset, start int64
	var length int
	var body strings.Builder
	for {
		line, n, err := readRecord(source, false)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if mysqlSlowHeader.MatchString(strings.TrimPrefix(line, "\ufeff")) {
			if length > 0 {
				if err := accept(decodeMySQLSlow(body.String(), start, length, zone)); err != nil {
					return err
				}
			}
			body.Reset()
			start, length = offset, n
			body.WriteString(strings.TrimPrefix(line, "\ufeff"))
		} else if length > 0 {
			length += n
			if length <= MaxRecordBytes {
				body.WriteString(line)
			}
		}
		offset += int64(n)
		if n == 0 || errors.Is(err, io.EOF) {
			break
		}
	}
	if length > 0 {
		return accept(decodeMySQLSlow(body.String(), start, length, zone))
	}
	return nil
}

func decodeMySQLSlow(body string, start int64, length int, zone *time.Location) Record {
	record := Record{Offset: start, Length: length, Kind: "unknown"}
	if length > MaxRecordBytes {
		return record
	}
	record.At = parseTime(match(mysqlSlowHeader, body), zone)
	elapsed := duration(match(mysqlSlowDuration, body))
	if record.At.IsZero() || elapsed == nil {
		return record
	}
	millis := *elapsed * 1000
	record.DurationMS, record.Kind, record.Severity = &millis, "statement", "LOG"
	record.Process = match(mysqlSlowProcess, body)
	record.Database = match(mysqlSlowSchema, body)
	var sql strings.Builder
	readingSQL := false
	for line := range strings.SplitSeq(body, "\n") {
		if !readingSQL {
			line = strings.TrimSpace(line)
			switch {
			case line == "" || strings.HasPrefix(line, "#"):
				continue
			case mysqlSlowStart.MatchString(line):
				readingSQL = true
				continue
			case mysqlSlowUse.MatchString(line):
				record.Database = match(mysqlSlowUse, line)
				continue
			default:
				readingSQL = true
			}
		}
		sql.WriteString(line)
		sql.WriteByte('\n')
	}
	record.Shape, record.Fingerprint = SQLShape("mysql", sql.String())
	if words := strings.Fields(record.Shape); len(words) > 0 {
		record.Command = strings.ToUpper(words[0])
	}
	return record
}
