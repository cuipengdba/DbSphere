package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStatementWindowUsesOnlyComparableIntervals(t *testing.T) {
	const fingerprint = "1:2:3:1"
	from := time.Now().UTC()
	window := StatementWindow{From: from, To: from.Add(time.Hour)}
	target := statementEvent{Engine: "postgresql", Database: "postgres"}
	sample := func(id string, seconds int, calls, total float64, reset string) sampleObservation {
		return sampleObservation{ID: id, At: from.Add(time.Duration(seconds) * time.Second), Status: "ok", Metrics: map[string]any{
			"databaseName": "postgres", "postgresEndpoint": map[string]any{"port": float64(5432)},
			"sqlPerformance": sqlRegressionFixture(calls, total, reset),
		}}
	}
	start, end := sample("start", 0, 1, 10, "a"), sample("end", 20, 6, 85, "a")
	got := summarizeStatementWindow(window, target, fingerprint, []sampleObservation{start, end})
	if got.Status != "ok" || got.Calls != 5 || got.AverageMS == nil || *got.AverageMS != 15 || got.FirstSampleID != "start" || got.LastSampleID != "end" {
		t.Fatalf("unexpected comparison: %+v", got)
	}
	for _, tc := range []struct {
		name   string
		next   sampleObservation
		status string
	}{
		{"counter reset", sample("reset", 20, 7, 120, "b"), "reset"},
		{"low calls", sample("few", 20, 4, 120, "a"), "low_calls"},
		{"long gap", sample("gap", 601, 7, 120, "a"), "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if result := summarizeStatementWindow(window, target, fingerprint, []sampleObservation{start, tc.next}); result.Status != tc.status {
				t.Fatalf("got %+v, want %s", result, tc.status)
			}
		})
	}
}

func TestMySQLStatementFirstSeenSeparatesReset(t *testing.T) {
	const digest = "fixture"
	before := []map[string]any{{"digest": digest, "firstSeen": "earlier", "calls": float64(10), "totalMs": float64(2)}}
	after := []map[string]any{{"digest": digest, "firstSeen": "later", "calls": float64(50), "totalMs": float64(8)}}
	if _, _, status := statementCounterDelta(before, after, "digest", digest, "totalMs"); status != "reset" {
		t.Fatalf("new digest incarnation treated as %s", status)
	}
	after[0]["firstSeen"] = "earlier"
	if calls, elapsed, status := statementCounterDelta(before, after, "digest", digest, "totalMs"); status != "ok" || calls != 40 || elapsed != 6 {
		t.Fatalf("bad delta: %v %v %s", calls, elapsed, status)
	}
}

func TestStatementPlanFormatsExcludeSQLAndObjectNames(t *testing.T) {
	mysql := json.RawMessage(`{"query_block":{"nested_loop":[{"table":{"access_type":"ref","table_name":"private_table","rows_examined_per_scan":3,"attached_condition":"private_value"}},{"table":{"access_type":"ALL","rows_examined_per_scan":99}}]}}`)
	nodes, identity, err := parseStatementPlan("mysql", "", mysql)
	encoded, _ := json.Marshal(nodes)
	if err != nil || identity != "operator" || len(nodes) != 2 || nodes[0].Operation != "ref" || nodes[1].Operation != "ALL" || strings.Contains(string(encoded), "private") {
		t.Fatalf("invalid plan result %s: %s %v", identity, encoded, err)
	}
	plan := json.RawMessage(`[{"Query Identifier":3,"Plan":{"Node Type":"Index Scan","Relation Name":"private_relation","Plan Rows":2}}]`)
	if nodes, identity, err := parseStatementPlan("postgresql", "3", plan); err != nil || identity != "explain" || len(nodes) != 1 || nodes[0].Operation != "Index Scan" {
		t.Fatalf("invalid PostgreSQL plan %+v: %s %v", nodes, identity, err)
	}
	if _, _, err := parseStatementPlan("postgresql", "4", plan); err == nil {
		t.Fatal("mismatched PostgreSQL Query Identifier accepted")
	}
}
