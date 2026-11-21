package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestReplayDoesNotPromoteSimultaneousHostPressureToDirectCause(t *testing.T) {
	started := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	ended := started.Add(time.Minute)
	event := &eventRecord{ID: "event-a", AssetID: "asset-a", StartedAt: started, EndedAt: &ended,
		Evidence: []eventEvidence{
			{ID: "lock", Kind: "alert_firing", ObservedAt: started, Payload: json.RawMessage(`{"sampleId":"a","metric":"pg.blocked_sessions"}`)},
			{ID: "cpu", Kind: "alert_firing", ObservedAt: started, Payload: json.RawMessage(`{"sampleId":"a","metric":"linux.cpu_percent"}`)},
			{ID: "recovery", Kind: "alert_recovered", ObservedAt: ended, Payload: json.RawMessage(`{"sampleId":"b","metric":"pg.blocked_sessions"}`)},
		}}
	samples := map[string]sampleObservation{
		"a": {ID: "a", At: started, Metrics: map[string]any{"k8s": nil}},
		"b": {ID: "b", At: ended, Metrics: map[string]any{"collector": "ok"}},
	}
	candidates := []map[string]any{
		{"code": "lock_chain", "supports": []string{"lock", "sample:a"}},
		{"code": "linux_cpu_pressure", "supports": []string{"cpu", "sample:a"}},
		{"code": "lock_chain", "supports": []string{"sample:a"}, "timeOrder": map[string]any{"relation": "after_event"}},
		{"code": "unknown", "supports": []string{}},
	}
	replay := incidentReplay(event, samples, candidates)
	want := []string{"direct", "correlated", "correlated", "insufficient"}
	for i, candidate := range candidates {
		if candidate["evidenceStrength"] != want[i] {
			t.Fatalf("candidate %d evidence strength: %v", i, candidate)
		}
	}
	if replay["eventId"] != event.ID || replay["detection"].(*replayPoint).SampleID != "a" || replay["recovery"].(*replayPoint).SampleID != "b" {
		t.Fatalf("replay lost the original event or recovery sample: %v", replay)
	}
}

func TestLegacyReplayLinksOnlyIdenticalStateAtTheExactSampleTime(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	payload := json.RawMessage(`{"status":"ok","pod":{"uid":"expected-uid"}}`)
	item := eventEvidence{Kind: "k8s_state", ObservedAt: at, Payload: payload}
	known := sampleObservation{ID: "current", At: at, Metrics: map[string]any{
		"k8s": map[string]any{"status": "ok", "pod": map[string]any{"uid": "expected-uid"}},
	}}
	other := sampleObservation{ID: "nearby", At: at, Metrics: map[string]any{
		"k8s": map[string]any{"status": "ok", "pod": map[string]any{"uid": "different-uid"}},
	}}
	if result := linkLegacyLayerEvidence(&item, map[int64][]sampleObservation{at.UnixNano(): {other}}); result != "" {
		t.Fatalf("unrelated state was linked at the same time: %s", result)
	}
	if result := linkLegacyLayerEvidence(&item, map[int64][]sampleObservation{at.UnixNano(): {known}}); result != known.ID {
		t.Fatalf("identical archived state did not link: %s", result)
	}
}
