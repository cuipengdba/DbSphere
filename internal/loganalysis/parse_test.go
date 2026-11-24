package loganalysis

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"
)

func TestPostgresAndHighGoFormats(t *testing.T) {
	var csvText strings.Builder
	writer := csv.NewWriter(&csvText)
	fields := make([]string, 23)
	fields[0], fields[2], fields[3], fields[5], fields[6] = "2026-11-24 19:10:00.123 UTC", "orders", "12", "100.12", "1"
	fields[11], fields[13], fields[22] = "LOG", "duration: 2100.5 ms statement: select id\nfrom orders where code='private'", "orders-api"
	if err := writer.Write(fields); err != nil {
		t.Fatal(err)
	}
	writer.Flush()
	pgJSON := `{"timestamp":"2026-11-24 19:10:00.123 UTC","dbname":"orders","pid":12,"session_id":"100.12","line_num":1,"application_name":"orders-api","error_severity":"LOG","message":"duration: 2100.5 ms statement: select id from orders where code=$1"}` + "\n"
	pgText := "2026-11-25 03:10:00.123 [12] orders-api orders LOG: duration: 2100.5 ms statement: select id\nfrom orders where code='private'\n"
	tz, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct{ input, format, prefix string }{
		{csvText.String(), "csvlog", ""},
		{pgJSON, "jsonlog", ""},
		{pgText, "text", "%m [%p] %a %d "},
	}
	var canonical string
	for _, test := range tests {
		var records []Record
		format, err := Parse(strings.NewReader(test.input), Options{Engine: "highgo", Format: "auto", Zone: tz, Prefix: test.prefix},
			func(record Record) error { records = append(records, record); return nil })
		if err != nil || format != test.format || len(records) != 1 {
			t.Fatalf("%s parsed %d records, format %s: %v", test.format, len(records), format, err)
		}
		record := records[0]
		if record.Offset != 0 || record.Length != len(test.input) || record.Kind != "statement" ||
			record.DurationMS == nil || *record.DurationMS != 2100.5 || record.Application != "orders-api" ||
			record.At.UTC().Format(time.RFC3339Nano) != "2026-11-24T19:10:00.123Z" || strings.Contains(record.Shape, "private") {
			t.Fatalf("%s: %+v", test.format, record)
		}
		if canonical != "" && canonical != record.Fingerprint {
			t.Fatalf("fingerprint changed with format: %s", record.Shape)
		}
		canonical = record.Fingerprint
	}
}

func TestStatementRedactionAndScope(t *testing.T) {
	a, fp := SQLShape("postgres", "SELECT a FROM \"Payments\" WHERE user_id=42 AND code=$tag$secret$tag$ /* private */")
	b, other := SQLShape("postgres", "select a from \"Payments\" where user_id=$1 and code=E'secret'")
	if a != b || fp != other || strings.Contains(a, "secret") || strings.Contains(a, "private") {
		t.Fatalf("redaction mismatch: %q and %q", a, b)
	}
	_, vendor := SQLShape("highgo", "select a from \"Payments\" where user_id=$1 and code=E'secret'")
	if fp == vendor {
		t.Fatal("fingerprint must stay scoped to its dialect")
	}
}
