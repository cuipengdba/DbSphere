package loganalysis

import (
	"strings"
	"testing"
)

func TestMySQLSlowLogRetainsByteRangesAndRedactsLiterals(t *testing.T) {
	const banner = "/usr/sbin/mysqld, Version: 8.4.13 started with:\nTime Id Command Argument\n"
	const first = `# Time: 2026-11-25T12:03:04.004382Z
# User@Host: admin[admin] @ test []  Id: 14
# Query_time: 1.420000  Lock_time: 0.100000 Rows_sent: 2 Rows_examined: 3
use app;
SET timestamp=1795608184;
SELECT "private value",0xABC123 FROM item WHERE id=5;
`
	const second = `# Time: 2026-11-25T12:03:05.004383Z
# User@Host: admin[admin] @ test []  Id: 14
# Query_time: 1.230000  Lock_time: 0.000000 Rows_sent: 1 Rows_examined: 2
use app;
SET timestamp=1795608185;
SELECT "different value",0xDEFE63 FROM item WHERE id=15;`
	var records []Record
	format, err := Parse(strings.NewReader(banner+first+second), Options{Engine: "mysql", Format: "auto"}, func(record Record) error {
		records = append(records, record)
		return nil
	})
	if err != nil || format != "mysqlslow" || len(records) != 2 {
		t.Fatalf("wrong MySQL slow log decoding: %s, %v, %d records", format, err, len(records))
	}
	a, b := records[0], records[1]
	if a.Offset != int64(len(banner)) || a.Length != len(first) || b.Offset != int64(len(banner)+len(first)) || b.Length != len(second) {
		t.Fatal("slow log record ranges moved")
	}
	if a.Kind != "statement" || a.Database != "app" || a.Process != "14" || a.DurationMS == nil || *a.DurationMS != 1420 ||
		a.Fingerprint == "" || a.Fingerprint != b.Fingerprint || strings.Contains(a.Shape, "private") || strings.Contains(a.Shape, "ABC123") {
		t.Fatalf("wrong slow log shape or headers: %+v", a)
	}
}
