package store

import (
	"strings"
	"testing"
	"time"
)

func TestTraceImpactRequiresDatabaseFailureOrExactLockWait(t *testing.T) {
	at := time.Now().UTC()
	errorTrace := strings.Repeat("a", 32)
	healthyTrace := strings.Repeat("b", 32)
	lockTrace := strings.Repeat("c", 32)
	spans := []observedTraceSpan{
		{TraceID: errorTrace, SpanID: "parent", Service: "gateway", Start: at, End: at.Add(time.Second)},
		{TraceID: errorTrace, SpanID: "db", ParentSpanID: "parent", Service: "orders",
			Kind: "postgresql_client", Status: "error", Start: at, End: at.Add(time.Second)},
		{TraceID: healthyTrace, SpanID: "parent", Service: "gateway", Start: at, End: at.Add(time.Second)},
		{TraceID: healthyTrace, SpanID: "db", ParentSpanID: "parent", Service: "orders",
			Kind: "postgresql_client", Status: "ok", Start: at, End: at.Add(time.Second)},
		{TraceID: lockTrace, SpanID: "parent", Service: "payments", Start: at, End: at.Add(time.Second)},
		{TraceID: lockTrace, SpanID: "db", ParentSpanID: "parent", Service: "payments",
			Kind: "postgresql_client", Status: "ok", BackendPID: 123, Start: at, End: at.Add(time.Second)},
	}
	samples := map[string]sampleObservation{"lock": {Status: "ok", At: at.Add(500 * time.Millisecond),
		Metrics: map[string]any{"diagnostics": map[string]any{"lockChain": map[string]any{
			"status": "ok", "data": []any{map[string]any{"waitingPid": float64(123), "blockerPid": float64(456)}},
		}}}}}
	impacts := map[string]*serviceTraceImpact{"gateway": {Service: "gateway"},
		"orders": {Service: "orders"}, "payments": {Service: "payments"}}
	classifyTraceImpact(spans, samples, impacts, nil)
	for _, service := range []string{"gateway", "orders"} {
		impact := impacts[service]
		if impact.ObservedTraces != 2 || impact.CorrelatedTraces != 1 || impact.ErrorTraces != 1 ||
			impact.LockWaitTraces != 0 || len(impact.TraceIDs) != 1 || impact.TraceIDs[0] != errorTrace {
			t.Fatalf("%s counted unrelated healthy request: %+v", service, impact)
		}
	}
	if impact := impacts["payments"]; impact.ObservedTraces != 1 || impact.CorrelatedTraces != 1 ||
		impact.LockWaitTraces != 1 || impact.ErrorTraces != 0 || len(impact.TraceIDs) != 1 ||
		impact.TraceIDs[0] != lockTrace {
		t.Fatalf("exact PG lock waiter was not associated: %+v", impact)
	}
	spans[len(spans)-1].BackendPID = 999
	impacts = map[string]*serviceTraceImpact{"payments": {Service: "payments"}}
	classifyTraceImpact(spans[4:], samples, impacts, nil)
	if impact := impacts["payments"]; impact.ObservedTraces != 1 || impact.CorrelatedTraces != 0 {
		t.Fatalf("different backend PID was associated with the request: %+v", impact)
	}
}

func TestTraceImpactMatchesOnlySlowCallForAlertedSQLFingerprint(t *testing.T) {
	at := time.Now().UTC()
	event := &eventRecord{Evidence: []eventEvidence{{Kind: "alert_firing",
		Payload: []byte(`{"metric":"pg.sql_regression_ratio","detail":{"worst":{"queryId":"42","fingerprint":"1:2:42:1","previousAverageExecMs":20,"currentAverageExecMs":100}}}`)}}}
	regression := sqlRegressionForEvent(event)
	if regression == nil {
		t.Fatal("alerted SQL fingerprint was not parsed")
	}
	samples := map[string]sampleObservation{"current": {Status: "ok", At: at,
		Metrics: map[string]any{"sqlPerformance": map[string]any{"status": "ok", "statements": []any{
			map[string]any{"queryId": "42", "fingerprint": "1:2:42:1"}}}}}}
	spans := []observedTraceSpan{
		{TraceID: "slow", SpanID: "parent", Service: "gateway", Start: at, End: at.Add(150 * time.Millisecond)},
		{TraceID: "slow", SpanID: "db", ParentSpanID: "parent", Service: "checkout",
			Kind: "postgresql_client", QueryID: "42", Start: at, End: at.Add(120 * time.Millisecond)},
		{TraceID: "fast", SpanID: "db", Service: "checkout", Kind: "postgresql_client",
			QueryID: "42", Start: at, End: at.Add(30 * time.Millisecond)},
		{TraceID: "other", SpanID: "db", Service: "checkout", Kind: "postgresql_client",
			QueryID: "99", Start: at, End: at.Add(120 * time.Millisecond)},
	}
	impacts := map[string]*serviceTraceImpact{"gateway": {Service: "gateway"}, "checkout": {Service: "checkout"}}
	classifyTraceImpact(spans, samples, impacts, regression)
	if impacts["gateway"].SQLRegressionTraces != 1 || impacts["gateway"].CorrelatedTraces != 1 ||
		impacts["checkout"].SQLRegressionTraces != 1 || impacts["checkout"].CorrelatedTraces != 1 ||
		impacts["checkout"].ObservedTraces != 3 {
		t.Fatalf("SQL signal did not propagate precisely to the request: %+v", impacts)
	}
	samples["other_role"] = sampleObservation{Status: "ok", At: at, Metrics: map[string]any{
		"sqlPerformance": map[string]any{"status": "ok", "statements": []any{
			map[string]any{"queryId": "42", "fingerprint": "1:3:42:1"}}}}}
	impacts = map[string]*serviceTraceImpact{"gateway": {Service: "gateway"}, "checkout": {Service: "checkout"}}
	classifyTraceImpact(spans, samples, impacts, regression)
	if impacts["gateway"].CorrelatedTraces != 0 || impacts["checkout"].CorrelatedTraces != 0 {
		t.Fatalf("ambiguous Query ID was counted as an affected request: %+v", impacts)
	}
}
