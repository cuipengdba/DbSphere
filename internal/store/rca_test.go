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
		metric("e7", "pgbouncer.waiting_clients", 2), metric("e8", "pg.connection_utilization", 99),
		metric("e9", "k8s.restart_delta", 1)}}
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
		"k8s_scheduling_event", "k8s_pod_restart", "pvc_pressure", "linux_disk_pressure", "pooler_queue"} {
		if !found[code] {
			t.Fatalf("cross-layer candidate %s was lost when PG failed: %+v", code, found)
		}
	}
	if found["connection_pressure"] {
		t.Fatal("PostgreSQL root cause inferred without a PostgreSQL sample")
	}
}

func TestGoRCAProducesOOMCandidateWithoutRestartAlert(t *testing.T) {
	now := time.Now().UTC()
	payload, _ := json.Marshal(map[string]any{"metric": "k8s.oom_recent", "sampleId": "sample-oom",
		"value": 1, "operator": ">=", "threshold": 1})
	event := &eventRecord{ID: "event-oom", StartedAt: now, Evidence: []eventEvidence{{
		ID: "evidence-oom", Kind: "alert_firing", ObservedAt: now, Payload: payload}}}
	sample := sampleObservation{ID: "sample-oom", At: now, Status: "error", Metrics: map[string]any{
		"k8s": map[string]any{"status": "ok", "pod": map[string]any{"name": "pg-0"},
			"containers": []any{map[string]any{"name": "postgres", "terminatedReason": "OOMKilled",
				"lastTerminatedAt": now.Add(-time.Minute).Format(time.RFC3339Nano)}}}}}
	candidates := analyzeCandidates(event, map[string]sampleObservation{"sample-oom": sample},
		func(string, string, string, string, any) {}, &[]map[string]string{})
	if len(candidates) != 1 || candidates[0]["code"] != "k8s_oom_killed" ||
		len(candidates[0]["supports"].([]string)) != 2 {
		t.Fatalf("standalone OOM alert lost its same-event candidate: %+v", candidates)
	}
	noOOM := sample
	noOOM.Metrics = map[string]any{"k8s": map[string]any{"status": "ok", "pod": map[string]any{"name": "pg-0"},
		"containers": []any{map[string]any{"name": "postgres", "terminatedReason": "Error",
			"lastTerminatedAt": now.Add(-time.Minute).Format(time.RFC3339Nano)}}}}
	if candidates := analyzeCandidates(event, map[string]sampleObservation{"sample-oom": noOOM},
		func(string, string, string, string, any) {}, &[]map[string]string{}); len(candidates) != 0 {
		t.Fatalf("non-OOM termination became an OOM candidate: %+v", candidates)
	}
}

func TestGoRCADeepPostgresCandidateEvidence(t *testing.T) {
	now := time.Now().UTC()
	alert := func(id, metric, sampleID string, value float64) eventEvidence {
		body, _ := json.Marshal(map[string]any{"metric": metric, "sampleId": sampleID,
			"value": value, "operator": ">=", "threshold": 1})
		return eventEvidence{ID: id, Kind: "alert_firing", ObservedAt: now, Payload: body}
	}
	event := &eventRecord{ID: "event-2", StartedAt: now, Evidence: []eventEvidence{
		alert("e1", "pg.replication_lag_bytes", "primary", 70*1024*1024),
		alert("e2", "pg.wal_receiver_down", "standby", 1),
		alert("e3", "pg.autovacuum_disabled", "primary", 1),
		alert("e4", "pg.vacuum_backlog_percent", "primary", 25),
		alert("e5", "pg.archive_fail_delta", "primary", 1)}}
	samples := map[string]sampleObservation{
		"primary": {ID: "primary", At: now, Status: "ok", Metrics: map[string]any{
			"role": "primary", "replication_lag_bytes": float64(70 * 1024 * 1024),
			"diagnostics": map[string]any{"vacuum": map[string]any{"status": "ok", "data": []any{
				map[string]any{"deadRowsEstimate": float64(20000)}}},
				"archiver": map[string]any{"status": "ok"}}}},
		"standby": {ID: "standby", At: now, Status: "ok", Metrics: map[string]any{
			"role": "standby", "replication_observed": true, "wal_receiver": "absent"}}}
	candidates := analyzeCandidates(event, samples, func(string, string, string, string, any) {},
		&[]map[string]string{})
	found := map[string]bool{}
	for _, candidate := range candidates {
		found[candidate["code"].(string)] = true
		if len(candidate["supports"].([]string)) != 2 {
			t.Fatalf("candidate lacks alert and raw sample: %+v", candidate)
		}
	}
	for _, code := range []string{"replication_lag", "wal_receiver_down", "wal_archive_failures",
		"autovacuum_disabled", "vacuum_backlog"} {
		if !found[code] {
			t.Fatalf("PostgreSQL candidate %s was not proposed: %+v", code, found)
		}
	}
}
