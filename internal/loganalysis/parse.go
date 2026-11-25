package loganalysis

import (
	"bufio"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const MaxRecordBytes = 1 << 20

var (
	ErrFormat           = errors.New("unknown log format")
	pgTimestamp         = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?: ?(?:Z|[+-]\d\d(?::?\d\d)?|[A-Z]{2,6}))?`)
	pgSeverity          = regexp.MustCompile(`\b(DEBUG[1-5]?|INFO|NOTICE|WARNING|ERROR|LOG|FATAL|PANIC):\s*`)
	pgDuration          = regexp.MustCompile(`(?i)^duration:\s*(\d+(?:\.\d+)?)\s*ms\b(?:\s+(?:statement|execute[^:]*):\s*)?`)
	pgStatement         = regexp.MustCompile(`(?i)^(?:statement|execute[^:]*):\s*`)
	pgProcess           = regexp.MustCompile(`\[(\d+)\]`)
	pgBlocker           = regexp.MustCompile(`(?i)\bprocess(?:es)? holding the lock:\s*(\d+)\b`)
	pgWaitTime          = regexp.MustCompile(`(?i)\bafter\s+(\d+(?:\.\d+)?)\s*ms`)
	pgLockObject        = regexp.MustCompile(`(?i)\b([A-Za-z]+Lock) on (relation|tuple|transaction|page) ([0-9(),]+)(?: of database ([0-9]+))?`)
	pgCheckpointBuffers = regexp.MustCompile(`(?i)checkpoint complete: wrote (\d+) buffers`)
	pgDetailLine        = regexp.MustCompile(`(?m)^\s*(?:DETAIL|CONTEXT|HINT|LOCATION|STATEMENT):\s`)
	pgTemp              = regexp.MustCompile(`(?i)\btemporary file:.*\bsize\s+(\d+)\b`)
	pgApplication       = regexp.MustCompile(`(?:^|[ ,])app(?:lication_name)?=([^,\[\]\s]+)`)
	pgDatabase          = regexp.MustCompile(`(?:^|[ ,])db(?:name)?=([^,\[\]\s]+)`)
	pgField             = regexp.MustCompile(`%[a-zA-Z%]`)
)

type Options struct {
	Engine, Format, Prefix string
	Zone                   *time.Location
}

type Record struct {
	Offset                                    int64
	Length                                    int
	At                                        time.Time
	Kind, Severity, Database, Application     string
	Session, Line, Process, Blocker, SQLState string
	Shape, Fingerprint, Command, Identity     string
	DurationMS                                *float64
	ObjectKey                                 string
	CheckpointBuffers                         *int64
	TempBytes                                 int64
}

type Preview struct {
	Format, Reason string
}

func Detect(sample []byte) Preview {
	if mysqlSlowHeader.Match(sample) && mysqlSlowDuration.Match(sample) {
		return Preview{"mysqlslow", "MySQL 慢日志时间和执行耗时"}
	}
	first := strings.TrimSpace(strings.SplitN(strings.TrimPrefix(string(sample), "\ufeff"), "\n", 2)[0])
	if looksLikeDMTrace(first) {
		return Preview{"dmtrace", "达梦会话头和跟踪字段"}
	}
	if strings.HasPrefix(first, "{") {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(first), &obj) == nil && obj["timestamp"] != nil && obj["message"] != nil {
			return Preview{"jsonlog", "timestamp 和 message 字段"}
		}
	}
	if fields, err := csv.NewReader(strings.NewReader(string(sample))).Read(); err == nil && len(fields) >= 14 && pgTimestamp.MatchString(strings.TrimPrefix(fields[0], "\ufeff")) {
		return Preview{"csvlog", "CSV 时间字段及日志列"}
	}
	if pgTimestamp.MatchString(first) || pgSeverity.MatchString(first) {
		return Preview{"text", "文本时间戳或日志级别"}
	}
	return Preview{"", "请选择产品、日志格式和前缀"}
}

func Parse(reader io.Reader, opts Options, accept func(Record) error) (string, error) {
	source := bufio.NewReaderSize(reader, 64<<10)
	prefix, err := compilePrefix(opts.Prefix)
	if err != nil {
		return "", err
	}
	format := opts.Format
	if format == "" || format == "auto" {
		sample, err := source.Peek(8192)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		format = Detect(sample).Format
		if opts.Engine == "dm8" && format == "text" {
			format = "dmtrace"
		}
		if format == "" && prefix != nil && prefix.Match(sample) {
			format = "text"
		}
	}
	if format == "mysqlslow" || opts.Engine == "mysql" {
		if format != "mysqlslow" || opts.Engine != "mysql" {
			return "", ErrFormat
		}
		return format, parseMySQLSlow(source, opts.Zone, accept)
	}
	if format != "csvlog" && format != "jsonlog" && format != "text" && format != "dmtrace" {
		return "", ErrFormat
	}
	if opts.Zone == nil {
		opts.Zone = time.UTC
	}
	var offset int64
	var current rawRecord
	for {
		line, length, err := readRecord(source, format == "csvlog")
		if err != nil && !errors.Is(err, io.EOF) {
			return format, err
		}
		if length == 0 {
			break
		}
		if (format == "text" || format == "dmtrace") && current.length > 0 && line != "" && !pgStartsRecord(line, prefix) {
			if current.length+length <= MaxRecordBytes {
				current.text += line
			}
			current.length += length
		} else {
			if current.length > 0 {
				if err := accept(decodeRecord(current, format, opts, prefix)); err != nil {
					return format, err
				}
			}
			current = rawRecord{line, offset, length}
		}
		offset += int64(length)
		if errors.Is(err, io.EOF) {
			break
		}
	}
	if current.length > 0 {
		return format, accept(decodeRecord(current, format, opts, prefix))
	}
	return format, nil
}

type rawRecord struct {
	text   string
	offset int64
	length int
}

func readRecord(source *bufio.Reader, csvMode bool) (string, int, error) {
	var value strings.Builder
	var quoted bool
	var total int
	for {
		part, err := source.ReadSlice('\n')
		total += len(part)
		if total <= MaxRecordBytes {
			value.Write(part)
		}
		if csvMode && strings.Count(string(part), "\"")%2 == 1 {
			quoted = !quoted
		}
		if err == bufio.ErrBufferFull || (err == nil && quoted) {
			continue
		}
		if total > MaxRecordBytes {
			return "", total, err
		}
		return value.String(), total, err
	}
}

func pgStartsRecord(line string, prefix *regexp.Regexp) bool {
	return pgTimestamp.MatchString(line) || (prefix != nil && prefix.MatchString(line))
}

func compilePrefix(prefix string) (*regexp.Regexp, error) {
	if prefix == "" {
		return nil, nil
	}
	indices := pgField.FindAllStringIndex(prefix, -1)
	var pattern strings.Builder
	pattern.WriteString("^")
	var previous int
	for _, at := range indices {
		pattern.WriteString(regexp.QuoteMeta(prefix[previous:at[0]]))
		key := prefix[at[0]+1 : at[1]]
		switch key {
		case "%":
			pattern.WriteString("%")
		case "q":
		default:
			value := `[^\r\n]*?`
			switch key {
			case "m", "t", "s":
				value = strings.TrimPrefix(pgTimestamp.String(), "^")
			case "p", "l":
				value = `\d+`
			}
			pattern.WriteString("(?P<" + key + ">" + value + ")")
		}
		previous = at[1]
	}
	pattern.WriteString(regexp.QuoteMeta(prefix[previous:]))
	pattern.WriteString(`\s*`)
	return regexp.Compile(pattern.String())
}

func decodeRecord(raw rawRecord, format string, opts Options, prefix *regexp.Regexp) Record {
	record := Record{Offset: raw.offset, Length: raw.length, Kind: "unknown"}
	if raw.text == "" || raw.length > MaxRecordBytes {
		return record
	}
	text := strings.TrimPrefix(strings.ToValidUTF8(raw.text, "�"), "\ufeff")
	if opts.Engine == "dm8" && (format == "dmtrace" || format == "text") {
		return decodeDM8(record, text, opts)
	}
	var timestamp, message, query, detail, command string
	switch format {
	case "csvlog":
		fields, err := csv.NewReader(strings.NewReader(text)).Read()
		if err != nil || len(fields) < 14 {
			return record
		}
		timestamp, record.Database, record.Process, record.Session, record.Line = fields[0], fields[2], fields[3], fields[5], fields[6]
		command, record.Severity, record.SQLState, message = fields[7], fields[11], fields[12], fields[13]
		detail, query, record.Application = field(fields, 14), field(fields, 19), field(fields, 22)
	case "jsonlog":
		var value struct {
			Timestamp   string          `json:"timestamp"`
			Message     string          `json:"message"`
			Query       string          `json:"statement"`
			Detail      string          `json:"detail"`
			Database    string          `json:"dbname"`
			Application string          `json:"application_name"`
			Session     string          `json:"session_id"`
			Line        json.RawMessage `json:"line_num"`
			PID         json.RawMessage `json:"pid"`
			Severity    string          `json:"error_severity"`
			State       string          `json:"state_code"`
			Command     string          `json:"ps"`
		}
		if json.Unmarshal([]byte(text), &value) != nil || value.Message == "" {
			return record
		}
		timestamp, message, query, detail = value.Timestamp, value.Message, value.Query, value.Detail
		record.Database, record.Application, record.Session = value.Database, value.Application, value.Session
		record.Line, record.Process = strings.Trim(string(value.Line), "\""), strings.Trim(string(value.PID), "\"")
		record.Severity, record.SQLState, command = value.Severity, value.State, value.Command
	case "text":
		message = strings.TrimSpace(text)
		if prefix != nil {
			if match := prefix.FindStringSubmatch(message); match != nil {
				timestamp = group(prefix, match, "m")
				if timestamp == "" {
					timestamp = group(prefix, match, "t")
				}
				record.Process, record.Session, record.Line = group(prefix, match, "p"), group(prefix, match, "c"), group(prefix, match, "l")
				record.Application, record.Database, record.SQLState = group(prefix, match, "a"), group(prefix, match, "d"), group(prefix, match, "e")
				message = strings.TrimSpace(message[len(match[0]):])
			}
		}
		if timestamp == "" {
			timestamp = pgTimestamp.FindString(message)
		}
		if at := pgSeverity.FindStringSubmatchIndex(message); at != nil {
			header := message[:at[0]]
			record.Severity = message[at[2]:at[3]]
			if record.Process == "" {
				record.Process = match(pgProcess, header)
			}
			if record.Application == "" {
				record.Application = match(pgApplication, header)
			}
			if record.Database == "" {
				record.Database = match(pgDatabase, header)
			}
			message = message[at[1]:]
		}
	}
	record.At = parseTime(strings.TrimSpace(timestamp), opts.Zone)
	if record.At.IsZero() {
		return record
	}
	classify(&record, message, detail, query, command, opts.Engine)
	if record.Session != "" && record.Line != "" && record.Line != "null" {
		record.Identity = digest(record.Database + "\x00" + record.Session + "\x00" + record.Line + "\x00" + record.At.Format(time.RFC3339Nano))
	}
	return record
}

func classify(record *Record, message, detail, query, command, engine string) {
	message = strings.TrimSpace(message)
	lower := strings.ToLower(message)
	record.Kind = "other"
	switch {
	case record.SQLState == "40P01" || strings.Contains(lower, "deadlock detected"):
		record.Kind = "deadlock"
	case strings.Contains(lower, "still waiting for") || strings.Contains(lower, "acquired") && strings.Contains(lower, "lock") && strings.Contains(lower, "after"):
		record.Kind = "lock"
		record.Blocker = match(pgBlocker, detail+"\n"+message)
		if groups := pgLockObject.FindStringSubmatch(message); len(groups) > 1 {
			record.ObjectKey = strings.Join(groups[1:], " ")
		}
		if ms := pgWaitTime.FindStringSubmatch(message); len(ms) == 2 {
			record.DurationMS = duration(ms[1])
		}
	case strings.HasPrefix(lower, "checkpoint "), strings.HasPrefix(lower, "restartpoint "):
		record.Kind = "checkpoint"
		if written, err := strconv.ParseInt(match(pgCheckpointBuffers, message), 10, 64); err == nil {
			record.CheckpointBuffers = &written
		}
	case strings.HasPrefix(lower, "automatic vacuum"), strings.HasPrefix(lower, "automatic analyze"):
		record.Kind = "autovacuum"
	case pgTemp.MatchString(message):
		record.Kind = "temp"
		record.TempBytes, _ = strconv.ParseInt(match(pgTemp, message), 10, 64)
	case record.Severity == "ERROR" || record.Severity == "FATAL" || record.Severity == "PANIC":
		record.Kind = "error"
	case pgDuration.MatchString(message):
		record.Kind = "statement"
		parsed := pgDuration.FindStringSubmatch(message)
		record.DurationMS = duration(parsed[1])
		if raw := strings.TrimSpace(message[len(parsed[0]):]); raw != "" {
			query = raw
		}
	case pgStatement.MatchString(message):
		record.Kind = "statement"
		query = pgStatement.ReplaceAllString(message, "")
	}
	if marker := pgDetailLine.FindStringIndex(query); marker != nil {
		query = query[:marker[0]]
	}
	record.Shape, record.Fingerprint = SQLShape(engine, query)
	record.Command = strings.ToUpper(strings.TrimSpace(command))
	if record.Command == "" && record.Shape != "" {
		record.Command = strings.ToUpper(strings.Fields(record.Shape)[0])
	}
}

func field(fields []string, index int) string {
	if index < len(fields) {
		return fields[index]
	}
	return ""
}

func match(pattern *regexp.Regexp, value string) string {
	if found := pattern.FindStringSubmatch(value); len(found) > 1 {
		return found[1]
	}
	return ""
}

func group(pattern *regexp.Regexp, result []string, name string) string {
	if at := pattern.SubexpIndex(name); at >= 0 {
		return result[at]
	}
	return ""
}

func parseTime(value string, zone *time.Location) time.Time {
	value = strings.Replace(value, ",", ".", 1)
	if zone == nil {
		zone = time.UTC
	}
	for _, layout := range []string{
		time.RFC3339Nano, "2006-01-02 15:04:05.999999999 -0700",
		"2006-01-02 15:04:05.999999999 -07:00", "2006-01-02 15:04:05.999999999 -07",
		"2006-01-02 15:04:05.999999999 MST", "2006-01-02 15:04:05.999999999",
	} {
		if at, err := time.ParseInLocation(layout, value, zone); err == nil {
			if name, off := at.Zone(); off == 0 && name != "UTC" && name != "GMT" && strings.HasSuffix(layout, "MST") {
				at = time.Date(at.Year(), at.Month(), at.Day(), at.Hour(), at.Minute(), at.Second(), at.Nanosecond(), zone)
			}
			return at.UTC()
		}
	}
	return time.Time{}
}

func duration(value string) *float64 {
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || n < 0 || n > 1e9 {
		return nil
	}
	return &n
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func ValidateOptions(opts Options) error {
	if opts.Format != "" && opts.Format != "auto" && opts.Format != "text" && opts.Format != "csvlog" &&
		opts.Format != "jsonlog" && opts.Format != "dmtrace" && opts.Format != "mysqlslow" {
		return ErrFormat
	}
	if opts.Format == "mysqlslow" && opts.Engine != "mysql" ||
		opts.Engine == "mysql" && opts.Format != "" && opts.Format != "auto" && opts.Format != "mysqlslow" {
		return ErrFormat
	}
	if len(opts.Prefix) > 400 {
		return fmt.Errorf("log prefix too long")
	}
	_, err := compilePrefix(opts.Prefix)
	return err
}
