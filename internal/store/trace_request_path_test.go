package store

import (
	"testing"
	"time"
)

func TestTraceRequestPathsPreserveIndependentEvidenceStates(t *testing.T) {
	start := time.Now().UTC().Add(-time.Minute)
	end := start.Add(time.Second)
	seenAt := end.Add(time.Second)
	probability := 0.25
	service := func(traceID, spanID, parent, name string) requestSpanObservation {
		return requestSpanObservation{observedTraceSpan: observedTraceSpan{TraceID: traceID, SpanID: spanID,
			ParentSpanID: parent, Service: name, Kind: "service", Start: start, End: end}, ReceivedAt: &seenAt}
	}
	db := func(traceID, parent string, pid int) requestSpanObservation {
		result := service(traceID, "db", parent, "checkout")
		result.Kind, result.BackendPID, result.Route = "postgresql_client", pid, "pgbouncer"
		return result
	}
	root := service("correct", "gateway", "", "checkout-gateway")
	root.Sampling = &probability
	wrongClock := db("time", "order", 41)
	wrongClock.TimingAnomaly = true
	records := []requestSpanObservation{
		root, service("correct", "order", "gateway", "checkout"), db("correct", "order", 41),
		service("mismatch", "order", "", "checkout"), db("mismatch", "order", 43),
		db("missing", "gateway", 41), service("time", "order", "", "checkout"), wrongClock,
		service("unreported", "order", "", "checkout"), db("unreported", "order", 0),
	}
	sample := sampleObservation{ID: "sample", At: start.Add(500 * time.Millisecond), Status: "ok", Metrics: map[string]any{
		"pgbouncer": map[string]any{"status": "ok"}, "diagnostics": map[string]any{"lockChain": map[string]any{
			"status": "ok", "data": []any{map[string]any{"waitingPid": 41., "blockerPid": 40.}},
		}},
	}}
	paths, limited := traceRequestPaths(records, map[string]sampleObservation{sample.ID: sample})
	if limited || len(paths) != 5 {
		t.Fatalf("unexpected request path count: %d", len(paths))
	}
	byID := map[string]requestPathEvidence{}
	for _, path := range paths {
		byID[path.TraceID] = path
	}
	correct := byID["correct"]
	if len(correct.Steps) != 3 || correct.Steps[0].Service != "checkout-gateway" ||
		correct.Steps[0].Sampling == nil || *correct.Steps[0].Sampling != probability || correct.Steps[1].Sampling != nil ||
		correct.ParentState != "complete" || correct.BackendState != "matched" || correct.BackendSample != "sample" ||
		correct.BlockerPID != 40 || correct.PoolerState != "pool_sample_observed" || correct.PoolerSample != "sample" {
		t.Fatalf("two-service trace lost distinct on-path source evidence: %+v", correct)
	}
	if byID["missing"].ParentState != "missing_parent" || byID["missing"].MissingParent != "gateway" ||
		byID["missing"].BackendState != "matched" || byID["mismatch"].BackendState != "mismatch" || byID["mismatch"].BackendSample != "sample" {
		t.Fatal("missing parents and mismatched backend PIDs were not kept independent")
	}
	if !byID["time"].ClockAnomaly || byID["time"].BackendState != "time_unreliable" || byID["time"].PoolerSample != "" ||
		byID["unreported"].BackendState != "pid_unreported" || byID["unreported"].PoolerSample == "" {
		t.Fatal("unreliable timing or absent PID was replaced with unverified database evidence")
	}
}
