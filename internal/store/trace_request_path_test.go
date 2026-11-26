package store

import (
	"fmt"
	"slices"
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

func TestRequestInfrastructureUsesNearestVerifiedIdentityAndSourceTime(t *testing.T) {
	start := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	span := func(id, cluster, reportedUID, verifiedUID, claim, volume string, offset time.Duration) requestSpanObservation {
		at := start.Add(offset)
		return requestSpanObservation{observedTraceSpan: observedTraceSpan{TraceID: id, SpanID: "db", Kind: "postgresql_client", Start: at, End: at.Add(time.Second)},
			ReportedPod: []byte(fmt.Sprintf(`{"namespace":"apps","name":"checkout","uid":%q}`, reportedUID)),
			VerifiedPod: []byte(fmt.Sprintf(`{"namespace":"apps","name":"checkout","uid":%q,"cluster":%q,"createdAt":%q,"observedAt":%q,
			"pvcStates":[{"name":"data","claimUid":%q,"volumeName":"volume","volumeUid":%q,"createdAt":%q}]}`,
				verifiedUID, cluster, start.Add(-time.Minute).Format(time.RFC3339), at.Add(time.Second).Format(time.RFC3339),
				claim, volume, start.Add(-time.Minute).Format(time.RFC3339)))}
	}
	first := span("first", "local", "old", "old", "claim-1", "volume-1", 0)
	second := span("second", "local", "new", "new", "claim-2", "volume-2", 3*time.Second)
	third := span("third", "local", "new", "new", "claim-2", "volume-2", 6*time.Second)
	bad := span("bad", "local", "old", "new", "claim-2", "volume-2", 9*time.Second)
	other := span("other", "another", "other", "other", "other-claim", "other-volume", 5*time.Second)
	sample := func(id, pod string, offset, observed time.Duration) sampleObservation {
		return sampleObservation{ID: id, At: third.Start.Add(offset), Status: "ok", Metrics: map[string]any{
			"k8s": map[string]any{"status": "ok", "observedAt": third.Start.Add(observed).Format(time.RFC3339Nano),
				"namespace": "apps", "pod": map[string]any{"uid": pod, "name": "checkout"}},
		}}
	}
	paths, _ := traceRequestPaths([]requestSpanObservation{third, bad, first, other, second}, map[string]sampleObservation{
		"verified":    sample("verified", "new", 100*time.Millisecond, 100*time.Millisecond),
		"unrelated":   sample("unrelated", "old", 200*time.Millisecond, 200*time.Millisecond),
		"late_source": sample("late_source", "new", 300*time.Millisecond, 2*time.Second),
	})
	byID := map[string]requestInfrastructure{}
	for _, path := range paths {
		byID[path.TraceID] = path.Infrastructure
	}
	if !slices.Equal(byID["second"].Changes, []string{"pod_replaced", "pvc_replaced:data", "volume_replaced:volume"}) ||
		len(byID["third"].Changes) != 0 || byID["bad"].AppState != "reported" || byID["bad"].AppPod != nil {
		t.Fatalf("identity transitions or mismatch were not preserved: %+v", byID)
	}
	if samples := byID["third"].Samples; len(samples) != 1 || samples[0].SampleID != "verified" || samples[0].Relation != "same_pod_uid" {
		t.Fatalf("a later unrelated or out-of-window source displaced verified evidence: %+v", samples)
	}
}

func TestRequestPathAndGraphRejectIncompatibleParents(t *testing.T) {
	start := time.Date(2026, 11, 26, 0, 0, 0, 0, time.UTC)
	parent := requestSpanObservation{observedTraceSpan: observedTraceSpan{TraceID: "trace", SpanID: "parent", Start: start, End: start.Add(time.Second)}}
	child := requestSpanObservation{observedTraceSpan: observedTraceSpan{TraceID: "trace", SpanID: "child", ParentSpanID: "parent", Kind: "postgresql_client", Start: start.Add(2 * time.Second), End: start.Add(3 * time.Second)}}
	spans := []requestSpanObservation{parent, child}
	paths, _ := traceRequestPaths(spans, nil)
	if len(paths) != 1 || paths[0].ParentState != "time_conflict" || !paths[0].ClockAnomaly || len(paths[0].Steps) != 1 {
		t.Fatalf("path accepted two disjoint spans: %+v", paths)
	}
	if edges, pairs, conflicts := requestParentEdges(spans); len(edges) != 0 || len(pairs) != 0 || conflicts != 1 {
		t.Fatalf("graph accepted two disjoint spans: %v, %v, %d", edges, pairs, conflicts)
	}
	spans[0].End, spans[0].ParentSpanID = child.End, child.SpanID
	if edges, pairs, conflicts := requestParentEdges(spans); len(edges) != 0 || len(pairs) != 0 || conflicts != 2 {
		t.Fatalf("graph accepted a cycle: %v, %v, %d", edges, pairs, conflicts)
	}
}
