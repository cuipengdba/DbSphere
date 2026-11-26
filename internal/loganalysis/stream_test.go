package loganalysis

import "testing"

func TestCompleteStreamPrefix(t *testing.T) {
	for _, sample := range []struct{ format, complete, tail string }{
		{"jsonlog", "{\"message\":\"complete\"}\n", "{\"message\":\"partial"},
		{"csvlog", "a,\"line one\nline two\"\n", "a,\"unfinished\n"},
		{"text", "2026-11-26 12:00:00 UTC LOG: statement: SELECT\n1\n", "2026-11-26 12:00:01 UTC LOG: statement: SELECT\n"},
		{"mysqlslow", "# Time: 2026-11-26T12:00:00Z\n# Query_time: 1\nselect 1;\n", "# Time: 2026-11-26T12:00:01Z\n"},
	} {
		t.Run(sample.format, func(t *testing.T) {
			length, err := CompleteStreamPrefix([]byte(sample.complete+sample.tail), sample.format, "")
			if err != nil || length != len(sample.complete) {
				t.Fatalf("prefix length %d, want %d: %v", length, len(sample.complete), err)
			}
		})
	}
}
