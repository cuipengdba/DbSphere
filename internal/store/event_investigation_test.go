package store

import (
	"testing"
	"time"
)

func TestInvestigationPeakKeepsPVCIdentity(t *testing.T) {
	claim := func(name, uid string, used float64) map[string]any {
		return map[string]any{"name": name, "uid": uid,
			"usage": map[string]any{"status": "ok", "usedBytes": used, "capacityBytes": float64(100)}}
	}
	sample := func(claims ...any) sampleObservation {
		return sampleObservation{Metrics: map[string]any{"k8s": map[string]any{"status": "ok", "pvcs": claims}}}
	}
	anchor := sample(claim("data", "initial-uid", 87), claim("other", "other-uid", 80))
	_, measure := investigationPeakSignal("k8s.pvc_used_percent", anchor)
	if measure == nil {
		t.Fatal("expected a PVC-scoped measurement")
	}
	if value, ok := measure(sample(claim("data", "initial-uid", 91), claim("other", "other-uid", 99))); !ok || value != 91 {
		t.Fatalf("different claim changed the result: %v %v", value, ok)
	}
	if _, ok := measure(sample(claim("data", "replacement-uid", 99))); ok {
		t.Fatal("replacement PVC with reused name was counted")
	}
}

func TestInvestigationUsesOnlyTheFiringLockSample(t *testing.T) {
	at := time.Now()
	s := func(id string, waiting, blocker int) sampleObservation {
		return sampleObservation{ID: id, At: at, Status: "ok", Metrics: map[string]any{
			"diagnostics": map[string]any{"lockChain": map[string]any{"status": "ok",
				"data": []any{map[string]any{"waitingPid": waiting, "blockerPid": blocker}}}},
		}}
	}
	samples := map[string]sampleObservation{"nearby": s("nearby", 3, 4), "trigger": s("trigger", 10, 11)}
	points := []replayPoint{
		{Kind: "k8s_state", Strength: "direct", SampleID: "nearby", EvidenceID: "infrastructure"},
		{Kind: "alert_firing", Signal: "pg.blocked_sessions", Strength: "correlated", SampleID: "nearby", EvidenceID: "clock-gap"},
	}
	if result := investigationLockEvidence(points, samples); result != nil {
		t.Fatalf("unrelated evidence produced a shortcut: %+v", result)
	}
	points = append(points, replayPoint{Kind: "alert_firing", Signal: "pg.blocked_sessions",
		Strength: "direct", SampleID: "trigger", EvidenceID: "lock"})
	result := investigationLockEvidence(points, samples)
	if result == nil || result.SampleID != "trigger" || result.EvidenceID != "lock" ||
		len(result.Pairs) != 1 || result.Pairs[0].WaitingPID != 10 || result.Pairs[0].BlockerPID != 11 {
		t.Fatalf("the exact trigger and its lock relationship were not retained: %+v", result)
	}
}
