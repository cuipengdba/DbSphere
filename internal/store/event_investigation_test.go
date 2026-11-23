package store

import (
	"testing"
	"time"
)

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
