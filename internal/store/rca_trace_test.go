package store

import (
	"testing"
	"time"
)

func TestTraceLockCandidateRequiresParentAndObservedLock(t *testing.T) {
	now := time.Now().UTC()
	event := &eventRecord{ID: "event", StartedAt: now}
	span := traceDBSpan{Node: "span:trace:db", Parent: "span:trace:app", Service: "checkout",
		Start: now.Add(-time.Second), End: now}
	sample := sampleObservation{ID: "sample", At: now, Status: "ok", Metrics: map[string]any{
		"diagnostics": map[string]any{"lockChain": map[string]any{"status": "ok",
			"data": []any{map[string]any{"waitingPid": 1, "blockerPid": 2}}}}}}
	makeCandidate := func(spans []traceDBSpan, parents map[string]bool, observation sampleObservation) map[string]any {
		return traceLockCandidate(event, map[string]sampleObservation{"sample": observation}, spans, parents,
			func(string, string, string, string, any) {}, &[]map[string]string{})
	}
	parents := map[string]bool{span.Node: true, span.Parent: true}
	candidate := makeCandidate([]traceDBSpan{span}, parents, sample)
	if candidate == nil || candidate["code"] != "trace_lock_correlated" ||
		len(candidate["supports"].([]string)) != 3 {
		t.Fatalf("application-to-lock evidence was lost: %+v", candidate)
	}
	if makeCandidate([]traceDBSpan{span}, map[string]bool{span.Node: true}, sample) != nil {
		t.Fatal("orphan database span became a cross-layer candidate")
	}
	noLock := sample
	noLock.Metrics = map[string]any{"diagnostics": map[string]any{"lockChain": map[string]any{"status": "ok", "data": []any{}}}}
	if makeCandidate([]traceDBSpan{span}, parents, noLock) != nil {
		t.Fatal("trace without lock evidence became a lock candidate")
	}
	distant := sample
	distant.At = now.Add(10 * time.Minute)
	if makeCandidate([]traceDBSpan{span}, parents, distant) != nil {
		t.Fatal("distant lock sample became a trace candidate")
	}
}

func TestTraceLockCandidateMatchesReportedBackendPID(t *testing.T) {
	now := time.Now().UTC()
	event := &eventRecord{ID: "event", StartedAt: now}
	span := traceDBSpan{Node: "span:trace:db", Parent: "span:trace:app", Service: "checkout",
		Start: now.Add(-time.Second), End: now, BackendPID: 101}
	parents := map[string]bool{span.Node: true, span.Parent: true}
	makeSample := func(waiting int, at time.Time) sampleObservation {
		return sampleObservation{ID: "sample", At: at, Status: "ok", Metrics: map[string]any{
			"diagnostics": map[string]any{"lockChain": map[string]any{"status": "ok",
				"data": []any{map[string]any{"waitingPid": waiting, "blockerPid": 202}}}}}}
	}
	make := func(sample sampleObservation) (map[string]any, []map[string]string) {
		edges := []map[string]string{}
		candidate := traceLockCandidate(event, map[string]sampleObservation{"sample": sample},
			[]traceDBSpan{span}, parents, func(string, string, string, string, any) {}, &edges)
		return candidate, edges
	}
	candidate, edges := make(makeSample(101, now))
	if candidate == nil || candidate["code"] != "trace_lock_pid_matched" ||
		candidate["confidence"] != "medium" {
		t.Fatalf("exact backend PID did not strengthen the same-event candidate: %+v", candidate)
	}
	linked := false
	for _, edge := range edges {
		if edge["relation"] == "same_backend_pid" && edge["from"] == span.Node && edge["to"] == "sample:sample" {
			linked = true
		}
	}
	if !linked {
		t.Fatal("PID-matched trace did not point to raw sample")
	}
	if candidate, _ := make(makeSample(999, now)); candidate != nil {
		t.Fatal("different waiting PID became a direct application lock diagnosis")
	}
	if candidate, _ := make(makeSample(101, now.Add(30*time.Second))); candidate != nil {
		t.Fatal("distant PID sample became a direct application lock diagnosis")
	}
}
