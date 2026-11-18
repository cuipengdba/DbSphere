package store

import (
	"encoding/json"
	"strings"
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

func TestGoRCALateOOMDoesNotExplainInitialFailure(t *testing.T) {
	started := time.Now().UTC()
	alertAt := started.Add(8 * time.Minute)
	oomAt := started.Add(7 * time.Minute)
	payload, _ := json.Marshal(map[string]any{"metric": "k8s.oom_recent", "sampleId": "late-sample",
		"value": 1, "operator": ">=", "threshold": 1})
	event := &eventRecord{ID: "event-late-oom", StartedAt: started, Evidence: []eventEvidence{{
		ID: "late-alert", Kind: "alert_firing", ObservedAt: alertAt, Payload: payload}}}
	sample := sampleObservation{ID: "late-sample", At: alertAt, Status: "error", Metrics: map[string]any{
		"k8s": map[string]any{"status": "ok", "pod": map[string]any{"name": "pg-0"},
			"containers": []any{map[string]any{"name": "postgres", "terminatedReason": "OOMKilled",
				"lastTerminatedAt": oomAt.Format(time.RFC3339Nano)}}}}}
	candidates := analyzeCandidates(event, map[string]sampleObservation{"late-sample": sample},
		func(string, string, string, string, any) {}, &[]map[string]string{})
	if len(candidates) != 1 || candidates[0]["code"] != "k8s_oom_killed" ||
		candidates[0]["confidence"] != "low" ||
		len(candidates[0]["against"].([]string)) != 1 ||
		object(candidates[0]["timeOrder"])["relation"] != "after_event" {
		t.Fatalf("late OOM should remain a secondary hypothesis with counterevidence: %+v", candidates)
	}
	reportPayload, _ := json.Marshal(map[string]any{"event": event,
		"analysis": map[string]any{"status": "candidate", "candidates": candidates}})
	var report eventReportData
	if err := json.Unmarshal(reportPayload, &report); err != nil {
		t.Fatal(err)
	}
	markdown := renderEventMarkdown(report)
	if !strings.Contains(markdown, "置信级别：low") ||
		!strings.Contains(markdown, "反证：当前支撑信号的观测时间晚于事件开始超过 5 分钟") ||
		!strings.Contains(markdown, "after_event") {
		t.Fatalf("event report omitted late-cause uncertainty: %s", markdown)
	}
}

func TestGoRCAMergesCorrelatedOOMAndAlertEvidence(t *testing.T) {
	now := time.Now().UTC()
	alert, _ := json.Marshal(map[string]any{"metric": "k8s.oom_recent", "sampleId": "sample-oom",
		"value": 1, "operator": ">=", "threshold": 1})
	correlation, _ := json.Marshal(map[string]any{"hypothesis": "Pod OOMKilled 与 PG 连接失败同期",
		"confidence": "medium", "supports": []string{"collection-error", "k8s-state"},
		"causeObservedAt": now.Add(-time.Minute).Format(time.RFC3339Nano)})
	decision, _ := json.Marshal(map[string]any{"code": "pod_oom_correlated", "decision": "confirmed"})
	// The alert is deliberately ordered before the stored correlation evidence.
	event := &eventRecord{ID: "event-oom", StartedAt: now, Evidence: []eventEvidence{
		{ID: "oom-alert", Kind: "alert_firing", ObservedAt: now, Payload: alert},
		{ID: "oom-correlation", Kind: "root_cause_candidate", ObservedAt: now, Payload: correlation},
		{ID: "oom-decision", Kind: "rca_decision", ObservedAt: now, Payload: decision}}}
	sample := sampleObservation{ID: "sample-oom", At: now, Status: "error", Metrics: map[string]any{
		"k8s": map[string]any{"status": "ok", "pod": map[string]any{"name": "pg-0"},
			"containers": []any{map[string]any{"name": "postgres", "terminatedReason": "OOMKilled",
				"lastTerminatedAt": now.Add(-time.Minute).Format(time.RFC3339Nano)}}}}}
	edges := []map[string]string{}
	candidates := analyzeCandidates(event, map[string]sampleObservation{"sample-oom": sample},
		func(string, string, string, string, any) {}, &edges)
	if len(candidates) != 1 || candidates[0]["code"] != "pod_oom_correlated" ||
		len(candidates[0]["supports"].([]string)) != 5 ||
		len(candidates[0]["matchedConditions"].([]any)) != 1 ||
		object(candidates[0]["review"])["decision"] != "confirmed" {
		t.Fatalf("OOM evidence or existing review was lost: %+v", candidates)
	}
	for _, support := range []string{"oom-correlation", "collection-error", "k8s-state", "oom-alert", "sample:sample-oom"} {
		found := false
		for _, edge := range edges {
			if edge["from"] == "candidate:pod_oom_correlated" &&
				(edge["to"] == "evidence:"+support || edge["to"] == support) {
				found = true
			}
		}
		if !found {
			t.Fatalf("support %q was not linked to merged OOM candidate", support)
		}
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

func TestGoRCARequiresSameWindowGrowthAndStoragePressure(t *testing.T) {
	now := time.Now().UTC()
	alert := func(id, metric string, value float64, at time.Time) eventEvidence {
		body, _ := json.Marshal(map[string]any{"metric": metric, "sampleId": "sample-storage",
			"value": value, "operator": ">=", "threshold": 1})
		return eventEvidence{ID: id, Kind: "alert_firing", ObservedAt: at, Payload: body}
	}
	event := &eventRecord{ID: "storage-event", StartedAt: now, Evidence: []eventEvidence{
		alert("growth-evidence", "pg.database_growth_bytes_per_day", 12*1024*1024*1024, now),
		alert("disk-evidence", "linux.disk_used_percent", 94, now.Add(5*time.Minute))}}
	sample := sampleObservation{ID: "sample-storage", At: now, Status: "ok",
		Metrics: map[string]any{"databaseGrowth": map[string]any{"status": "ok",
			"baselineSampleId": "baseline-1"}, "linux": map[string]any{"status": "ok"}}}
	check := func() map[string]any {
		candidates := analyzeCandidates(event, map[string]sampleObservation{"sample-storage": sample},
			func(string, string, string, string, any) {}, &[]map[string]string{})
		for _, candidate := range candidates {
			if candidate["code"] == "storage_growth_pressure" {
				return candidate
			}
		}
		return nil
	}
	candidate := check()
	if candidate == nil || candidate["confidence"] != "low" ||
		object(candidate["detail"])["baselineSampleId"] != "baseline-1" ||
		len(candidate["supports"].([]string)) != 4 || len(candidate["missing"].([]string)) == 0 {
		t.Fatalf("same-event growth/pressure evidence was not correlated cautiously: %+v", candidate)
	}
	event.Evidence[1] = alert("pvc-evidence", "k8s.pvc_used_percent", 90, now.Add(5*time.Minute))
	sample.Metrics["k8s"] = map[string]any{"status": "ok"}
	if candidate := check(); candidate == nil ||
		object(candidate["detail"])["pressureMetric"] != "k8s.pvc_used_percent" {
		t.Fatalf("Kubernetes PVC pressure was not correlated with the same growth evidence: %+v", candidate)
	}
	event.Evidence[1].ObservedAt = now.Add(25 * time.Hour)
	if candidate := check(); candidate != nil {
		t.Fatalf("stale pressure became a root-cause correlation: %+v", candidate)
	}
	event.Evidence[1].ObservedAt = now.Add(5 * time.Minute)
	sample.Metrics["databaseGrowth"] = map[string]any{"status": "insufficient"}
	if candidate := check(); candidate != nil {
		t.Fatalf("growth without a baseline became a root-cause correlation: %+v", candidate)
	}
}
