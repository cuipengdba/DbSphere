package loganalysis

import (
	"strings"
	"testing"
	"time"
)

func TestDM8TrackedAndUntimedRecords(t *testing.T) {
	// DM8 SQL trace uses a session header, a statement tag and an optional EXECTIME suffix.
	source := "2026-11-24 11:30:40.021 (EP[0] sess:10 x123 thrd:311 user:APP trxid:22 stmt:2 x456 appname:orders-api ip:127.0.0.1) [SEL] select id from orders\nwhere code=q'[customer's private code]'; EXECTIME: 1350(us).\n" +
		"2026-11-24 11:30:41.052 select count(*) from orders;\n" +
		"2026-11-24 11:30:42.100 (EP[0] sess:14 x578 thrd:315 user:APP trxid:25 stmt:4 appname:orders-api ip:127.0.0.1) [ERR] select * from missing_table; ERRCODE: -2106\n" +
		"2026-11-24 11:30:43.523 (EP[0] sess:22 x913 thrd:317 user:APP trxid:31 stmt:2 appname:orders-api ip:127.0.0.1) [LOCK] lock wait waiter: 22 blocker: 10\n"
	var rows []Record
	format, err := Parse(strings.NewReader(source), Options{Engine: "dm8", Format: "auto", Zone: time.UTC},
		func(row Record) error { rows = append(rows, row); return nil })
	if err != nil || format != "dmtrace" || len(rows) != 4 {
		t.Fatalf("DM8 log recognition: format=%s records=%d error=%v", format, len(rows), err)
	}
	if rows[0].Application != "orders-api" || rows[0].DurationMS == nil || *rows[0].DurationMS != 1.35 ||
		rows[0].Kind != "statement" || strings.Contains(rows[0].Shape, "private") || rows[0].Fingerprint == "" {
		t.Fatalf("unexpected timed record: %+v", rows[0])
	}
	if rows[1].Kind != "statement" || rows[1].DurationMS != nil {
		t.Fatalf("untimed record needs a coverage gap: %+v", rows[1])
	}
	if rows[2].Kind != "error" || rows[2].SQLState != "-2106" ||
		rows[3].Kind != "lock" || rows[3].Process != "22" || rows[3].Blocker != "10" {
		t.Fatalf("error or lock evidence mismatch: %+v", rows)
	}
}
