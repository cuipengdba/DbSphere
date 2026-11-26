package loganalysis

import (
	"bytes"
)

// CompleteStreamPrefix returns bytes that can be archived and parsed without
// reading from the next chunk. Multi-line logs keep one record as lookahead.
func CompleteStreamPrefix(raw []byte, format, prefix string) (int, error) {
	matcher, err := compilePrefix(prefix)
	if err != nil {
		return 0, err
	}
	var safe, offset int
	var quoted, started bool
	for len(raw) > 0 {
		index := bytes.IndexByte(raw, '\n')
		if index < 0 {
			break
		}
		line := raw[:index+1]
		switch format {
		case "jsonlog":
			safe = offset + len(line)
		case "csvlog":
			if bytes.Count(line, []byte{'"'})%2 != 0 {
				quoted = !quoted
			}
			if !quoted {
				safe = offset + len(line)
			}
		case "text", "mysqlslow":
			isStart := mysqlSlowHeader.Match(bytes.TrimPrefix(line, []byte("\ufeff")))
			if format == "text" {
				isStart = pgStartsRecord(string(line), matcher)
			}
			if isStart {
				if started {
					safe = offset
				}
				started = true
			}
		default:
			return 0, ErrFormat
		}
		offset += len(line)
		raw = raw[len(line):]
	}
	return safe, nil
}
