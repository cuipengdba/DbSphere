package store

import (
	"encoding/json"
	"testing"
	"time"
)

func TestGoRCAPreservesCrossLayerCandidatesWhenPGFails(t *testing.T) {
	now := time.Now().UTC()
	metric := func(id, name string, value float64) eventEvidence {
		payload, _ := json.Marshal(map[string]any{"metric": name, "value": value,
			"sampleId": "sample-1", "operator": ">=", "threshold": 1})
		return eventEvidence{ID: id, Kind: "alert_firing", ObservedAt: now, Payload: payload}
	}
	event := &eventRecord{ID: "event-1", StartedAt: now, Evidence: []eventEvidence{
		metric("e1", "k8s.pod_not_ready", 1), metric("e2", "k8s.oom_recent", 1),
		metric("e3", "k8s.node_pressure", 1), metric("e4", "k8s.scheduling_failure_recent", 1),
		metric("e5", "k8s.pvc_used_percent", 90), metric("e6", "linux.disk_used_percent", 95),
		metric("e7", "pgbouncer.waiting_clients", 2), metric("e8", "pg.connection_utilization", 99)}}
	sample := sampleObservation{ID: "sample-1", At: now, Status: "error",
		Metrics: map[string]any{"k8s": map[string]any{"status": "ok",
			"pod": map[string]any{"name": "pg-0", "uid": "pod-uid", "conditions": []any{
				map[string]any{"type": "Ready", "status": "False"}}},
			"containers": []any{map[string]any{"name": "postgres", "terminatedReason": "OOMKilled",
				"lastTerminatedAt": now.Add(-time.Minute).Format(time.RFC3339Nano)}},
			"node": map[string]any{"name": "worker", "conditions": []any{
				map[string]any{"type": "MemoryPressure", "status": "True"}}},
			"events": []any{map[string]any{"reason": "FailedMount",
				"at": now.Add(-time.Minute).Format(time.RFC3339Nano)}}},
			"linux":     map[string]any{"status": "ok"},
			"pgbouncer": map[string]any{"status": "ok"}}}
	candidates := analyzeCandidates(event, map[string]sampleObservation{"sample-1": sample},
		func(string, string, string, string, any) {}, &[]map[string]string{})
	found := map[string]bool{}
	for _, candidate := range candidates {
		found[candidate["code"].(string)] = true
	}
	for _, code := range []string{"k8s_pod_not_ready", "k8s_oom_killed", "k8s_node_pressure",
		"k8s_scheduling_event", "pvc_pressure", "linux_disk_pressure", "pooler_queue"} {
		if !found[code] {
			t.Fatalf("cross-layer candidate %s was lost when PG failed: %+v", code, found)
		}
	}
	if found["connection_pressure"] {
		t.Fatal("PostgreSQL root cause inferred without a PostgreSQL sample")
	}
}
