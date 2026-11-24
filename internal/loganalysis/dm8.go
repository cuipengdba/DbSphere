package loganalysis

import (
	"regexp"
	"strings"
)

var (
	dmTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(?:[.,]\d+)?(?:Z|[+-]\d\d(?::?\d\d)?)?`)
	dmHeader    = regexp.MustCompile(`(?s)^\s*\((.*?)\)\s*\[([A-Za-z0-9_]+)\]\s*`)
	dmApp       = regexp.MustCompile(`(?i)\bappname:\s*(.*?)(?:\s+ip:|$)`)
	dmSession   = regexp.MustCompile(`(?i)\bsess:\s*([^\s)]+)`)
	dmProcess   = regexp.MustCompile(`(?i)\bthrd:\s*([^\s)]+)`)
	dmDuration  = regexp.MustCompile(`(?i)\s+EXECTIME:\s*(\d+(?:\.\d+)?)\s*(?:\((ms|us|µs|s)\)|(ms|us|µs|s))`)
	dmError     = regexp.MustCompile(`(?i)\b(?:ERR(?:OR)?CODE|ERRNO|ERROR CODE)\s*[:=]\s*(-\d+|[1-9]\d*)`)
	dmWaiter    = regexp.MustCompile(`(?i)\b(?:waiter|waiting\s+session)\s*[:=]\s*(\d+)`)
	dmBlocker   = regexp.MustCompile(`(?i)\b(?:blocker|blocking\s+session)\s*[:=]\s*(\d+)`)
	dmSQL       = regexp.MustCompile(`(?i)^(?:SELECT|WITH|INSERT|UPDATE|DELETE|MERGE|REPLACE|COMMIT|ROLLBACK|CREATE|DROP|ALTER|TRUNCATE|EXECUTE|CALL|BEGIN|DECLARE|SET)\b`)
	dmLogLevel  = regexp.MustCompile(`(?i)\b(?:\[)?(ERROR|WARN(?:ING)?|FATAL|INFO)(?:\]|:)`)
)

func looksLikeDMTrace(line string) bool {
	end := dmTimestamp.FindStringIndex(line)
	return end != nil && dmHeader.MatchString(line[end[1]:]) &&
		(strings.Contains(strings.ToLower(line), "sess:") || dmDuration.MatchString(line))
}

func decodeDM8(record Record, line string, opts Options) Record {
	end := dmTimestamp.FindStringIndex(line)
	if end == nil {
		return record
	}
	record.At = parseTime(strings.TrimSpace(line[:end[1]]), opts.Zone)
	if record.At.IsZero() {
		return record
	}
	body := strings.TrimSpace(line[end[1]:])
	tag := ""
	if header := dmHeader.FindStringSubmatch(body); header != nil {
		fields := header[1]
		record.Application = strings.TrimSpace(match(dmApp, fields))
		record.Session, record.Process = match(dmSession, fields), match(dmProcess, fields)
		tag = strings.ToUpper(header[2])
		body = strings.TrimSpace(body[len(header[0]):])
	}
	record.Kind = "other"
	record.SQLState = match(dmError, body)
	if tag == "ERR" || tag == "ERROR" || tag == "FATAL" || record.SQLState != "" {
		record.Kind, record.Severity = "error", "ERROR"
	}
	isStatement := dmSQL.MatchString(body)
	if isStatement {
		sql := body
		if elapsed := dmDuration.FindStringSubmatchIndex(body); elapsed != nil {
			record.DurationMS = duration(body[elapsed[2]:elapsed[3]])
			units := regexpSubstring(body, elapsed[4], elapsed[5])
			if units == "" {
				units = regexpSubstring(body, elapsed[6], elapsed[7])
			}
			if record.DurationMS != nil {
				switch strings.ToLower(units) {
				case "us", "µs":
					*record.DurationMS /= 1000
				case "s":
					*record.DurationMS *= 1000
				}
			}
			sql = body[:elapsed[0]]
		}
		if record.Kind == "other" {
			record.Kind = "statement"
		}
		record.Shape, record.Fingerprint = SQLShape("dm8", strings.TrimSpace(sql))
		if record.Shape != "" {
			record.Command = strings.ToUpper(strings.Fields(record.Shape)[0])
		}
	} else {
		level := strings.ToUpper(match(dmLogLevel, body))
		if level == "ERROR" || level == "FATAL" {
			record.Kind, record.Severity = "error", level
		}
		lower := strings.ToLower(body)
		switch {
		case strings.Contains(lower, "deadlock"):
			record.Kind = "deadlock"
		case tag == "LOCK" || strings.Contains(lower, "lock wait"):
			record.Kind = "lock"
		}
		if record.Kind == "lock" || record.Kind == "deadlock" {
			if waiter := match(dmWaiter, body); waiter != "" {
				record.Process = waiter
			}
			record.Blocker = match(dmBlocker, body)
		}
	}
	return record
}

func regexpSubstring(text string, start, end int) string {
	if start < 0 || end < 0 {
		return ""
	}
	return text[start:end]
}
