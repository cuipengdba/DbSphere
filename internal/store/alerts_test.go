package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestGoAlertPendingFiringMissingAndRecovery(t *testing.T) {
	rule := goDefaultRules[0]
	start := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	breach := alertSignal{Available: true, Value: 90}
	state, transition := nextGoAlert(rule, breach, nil, start)
	if state == nil || state.Status != "pending" || transition != "" {
		t.Fatal("initial breach did not enter pending state")
	}
	state, transition = nextGoAlert(rule, breach, state, start.Add(121*time.Second))
	if state == nil || state.Status != "firing" || transition != "firing" {
		t.Fatal("sustained breach did not fire")
	}
	state, transition = nextGoAlert(rule, alertSignal{}, state, start.Add(122*time.Second))
	if state == nil || state.Status != "firing" || transition != "signal_missing" {
		t.Fatal("missing signal cleared a firing alert")
	}
	state, transition = nextGoAlert(rule, alertSignal{Available: true, Value: 20}, state,
		start.Add(123*time.Second))
	if state != nil || transition != "recovered" {
		t.Fatal("normal signal did not recover the alert")
	}
}

func TestGoKubernetesAlertSignalsAndMissingData(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	metrics := map[string]any{"k8s": map[string]any{"status": "ok",
		"pod": map[string]any{"name": "pg-0", "uid": "pod-uid", "conditions": []any{
			map[string]any{"type": "Ready", "status": "False"},
			map[string]any{"type": "PodScheduled", "status": "True"}}},
		"containers": []any{map[string]any{"name": "postgres", "terminatedReason": "OOMKilled",
			"lastTerminatedAt": now.Add(-time.Minute).Format(time.RFC3339Nano)}},
		"events": []any{map[string]any{"reason": "FailedMount", "at": now.Add(-time.Minute).Format(time.RFC3339Nano)}},
		"node": map[string]any{"name": "worker-1", "conditions": []any{
			map[string]any{"type": "MemoryPressure", "status": "True"}}}}}
	for metric, want := range map[string]float64{"k8s.pod_not_ready": 1, "k8s.pod_unscheduled": 0,
		"k8s.oom_recent": 1, "k8s.scheduling_failure_recent": 1, "k8s.node_pressure": 1} {
		signal := readGoSignal(metric, metrics, now)
		if !signal.Available || signal.Value != want {
			t.Fatalf("%s signal mismatch: %+v", metric, signal)
		}
	}
	metrics["k8s"] = map[string]any{"status": "error", "reason": "Kubernetes API HTTP 403"}
	for metric := range map[string]bool{"k8s.pod_not_ready": true, "k8s.oom_recent": true,
		"k8s.node_pressure": true} {
		if readGoSignal(metric, metrics, now).Available {
			t.Fatalf("%s treated missing Kubernetes data as healthy", metric)
		}
	}
}

func TestGoPgBouncerWaitingSignal(t *testing.T) {
	now := time.Now().UTC()
	metrics := map[string]any{"pgbouncer": map[string]any{"status": "ok", "pools": []any{
		map[string]any{"clientWaiting": 2.0}, map[string]any{"clientWaiting": 1.0}}}}
	signal := readGoSignal("pgbouncer.waiting_clients", metrics, now)
	if !signal.Available || signal.Value != 3 || signal.Source != "PgBouncer" {
		t.Fatalf("PgBouncer waiting clients not aggregated: %+v", signal)
	}
	metrics["pgbouncer"] = map[string]any{"status": "error"}
	if readGoSignal("pgbouncer.waiting_clients", metrics, now).Available {
		t.Fatal("PgBouncer collection error treated as a healthy zero wait")
	}
}

func TestGoPVCUsageAlertRequiresRealUsedBytes(t *testing.T) {
	now := time.Now().UTC()
	metrics := map[string]any{"k8s": map[string]any{"status": "ok", "pvcs": []any{
		map[string]any{"name": "pg-data", "capacity": "10Gi",
			"usage": map[string]any{"status": "ok", "usedBytes": float64(9 * 1024 * 1024 * 1024)}}}}}
	signal := readGoSignal("k8s.pvc_used_percent", metrics, now)
	if !signal.Available || signal.Value != 90 || object(signal.Detail)["pvc"] != "pg-data" {
		t.Fatalf("PVC usage signal missing: %+v", signal)
	}
	object(list(object(metrics["k8s"])["pvcs"])[0])["usage"] = map[string]any{"status": "unavailable"}
	if readGoSignal("k8s.pvc_used_percent", metrics, now).Available {
		t.Fatal("missing PVC used bytes treated as a healthy zero")
	}
	object(list(object(metrics["k8s"])["pvcs"])[0])["usage"] = map[string]any{"status": "ok", "usedBytes": float64(1024)}
	object(metrics["k8s"])["pvcs"] = append(list(object(metrics["k8s"])["pvcs"]),
		map[string]any{"name": "missing-data", "capacity": "10Gi", "usage": map[string]any{"status": "unavailable"}})
	if readGoSignal("k8s.pvc_used_percent", metrics, now).Available {
		t.Fatal("partial PVC coverage treated as healthy")
	}
}

func TestGoLockWaitAndPoolerWaitShareIncident(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	created, err := pg.CreateAsset(context.Background(), assets.Input{Name: "Go lock and pooler test",
		Host: "127.0.0.1", Port: 5432, Database: "postgres", Username: "postgres",
		Password: "test-secret", SSLMode: "disable", Runtime: "linux"},
		[]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	metrics := map[string]any{"version_num": 180000, "connections": 1, "max_connections": 100,
		"diagnostics": map[string]any{"locks": map[string]any{"status": "ok",
			"data": map[string]any{"blockedSessions": 1}}},
		"pgbouncer": map[string]any{"status": "ok", "pools": []any{
			map[string]any{"clientWaiting": 2}}}}
	now := time.Now().UTC().Add(-2 * time.Minute)
	for _, offset := range []time.Duration{0, 61 * time.Second} {
		if _, err := pg.recordCheckAt(context.Background(), asset.ID, metrics, "", "", now.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}
	var eventID string
	var eventCount int
	if err := pg.pool.QueryRow(context.Background(), `SELECT count(*)::int,min(id::text) FROM events
		WHERE asset_id=$1 AND event_group='latency'`, asset.ID).Scan(&eventCount, &eventID); err != nil || eventCount != 1 {
		t.Fatalf("lock and pooler waits split into multiple incidents: %d %v", eventCount, err)
	}
	if err := pg.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM alert_states WHERE asset_id=$1
		AND state_json->>'eventId'=$2 AND state_json->>'status'='firing'`, asset.ID, eventID).
		Scan(&eventCount); err != nil || eventCount != 2 {
		t.Fatalf("lock and pooler alerts did not share event ID: %d %v", eventCount, err)
	}
	metrics["diagnostics"] = map[string]any{"locks": map[string]any{"status": "ok",
		"data": map[string]any{"blockedSessions": 0}}}
	metrics["pgbouncer"] = map[string]any{"status": "ok", "pools": []any{map[string]any{"clientWaiting": 0}}}
	if _, err := pg.recordCheckAt(context.Background(), asset.ID, metrics, "", "", now.Add(62*time.Second)); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pg.pool.QueryRow(context.Background(), `SELECT status FROM events WHERE id=$1`, eventID).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("lock/pooler incident did not resolve under original ID: %s %v", status, err)
	}
}

func TestGoAlertOneEventIDAndRCACandidateInPG18(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(context.Background(), assets.Input{Name: "Go alert lifecycle test",
		Host: "127.0.0.1", Port: 5432, Database: "postgres", Username: "postgres",
		Password: "test-secret", SSLMode: "disable", Runtime: "linux"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	now := time.Now().UTC().Add(-3 * time.Minute)
	pressure := map[string]any{"version_num": 180000, "connections": 90, "max_connections": 100}
	if _, err := pg.recordCheckAt(context.Background(), asset.ID, pressure, "", "", now); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pg.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM events
		WHERE asset_id=$1 AND event_group='capacity'`, asset.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("alert opened before its configured duration", count, err)
	}
	second, err := pg.recordCheckAt(context.Background(), asset.ID, pressure, "telemetry",
		"Linux node_exporter collection failed", now.Add(121*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if second.Status != "error" {
		t.Fatal("Linux collection error was not marked on the sample")
	}
	var eventID string
	if err := pg.pool.QueryRow(context.Background(), `SELECT id::text FROM events
		WHERE asset_id=$1 AND event_group='capacity' AND status='open'`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal("sustained connection pressure event missing", err)
	}
	detail, err := pg.EventByID(context.Background(), eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	if !strings.Contains(string(encoded), "connection_pressure") ||
		!strings.Contains(string(encoded), "supported_by") {
		t.Fatalf("Go alert did not produce an evidenced RCA candidate: %s", encoded)
	}
	pressure["connections"] = 20
	if _, err := pg.recordCheckAt(context.Background(), asset.ID, pressure, "", "", now.Add(122*time.Second)); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pg.pool.QueryRow(context.Background(), `SELECT status FROM events WHERE id=$1`, eventID).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("alert did not recover under the same event ID: %s %v", status, err)
	}
	if err := pg.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM evidence
		WHERE event_id=$1 AND kind='alert_recovered'`, eventID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovery evidence missing from event %s: %d %v", eventID, count, err)
	}
}

func TestUpdateGoAlertRulePersistsAndRejectsInvalid(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	rule := goDefaultRules[0]
	rule.ID = "go_rule_update_test"
	seed, _ := json.Marshal(rule)
	if _, err := pg.pool.Exec(context.Background(), `INSERT INTO alert_rules(id,definition_json) VALUES($1,$2)
		ON CONFLICT(id) DO UPDATE SET definition_json=excluded.definition_json`, rule.ID, seed); err != nil {
		t.Fatal(err)
	}
	defer pg.pool.Exec(context.Background(), `DELETE FROM alert_rules WHERE id=$1`, rule.ID)
	threshold := 92.0
	duration := 180
	enabled := false
	value, err := pg.UpdateAlertRule(context.Background(), rule.ID,
		AlertRulePatch{Threshold: &threshold, DurationSeconds: &duration, Enabled: &enabled})
	if err != nil || value == nil {
		t.Fatal("rule update failed", err)
	}
	var raw []byte
	if err := pg.pool.QueryRow(context.Background(), `SELECT definition_json FROM alert_rules WHERE id=$1`, rule.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var persisted alertRule
	if err := json.Unmarshal(raw, &persisted); err != nil || persisted.Threshold != threshold ||
		persisted.DurationSeconds != duration || persisted.Enabled || persisted.Version != 2 {
		t.Fatalf("alert rule update not persisted: %+v %v", persisted, err)
	}
	invalid := -1.0
	if _, err := pg.UpdateAlertRule(context.Background(), rule.ID,
		AlertRulePatch{Threshold: &invalid}); !errors.Is(err, ErrInvalidAlertRule) {
		t.Fatalf("negative threshold accepted: %v", err)
	}
	if _, err := pg.UpdateAlertRule(context.Background(), "go_missing_rule",
		AlertRulePatch{Threshold: &threshold}); !errors.Is(err, ErrAlertRuleNotFound) {
		t.Fatalf("unknown rule accepted: %v", err)
	}
}

func TestGoAlertGroupsPostgresAndLinuxUnderOneEvent(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(context.Background(), assets.Input{Name: "Go cross-layer alert test",
		Host: "127.0.0.1", Port: 5432, Database: "postgres", Username: "postgres",
		Password: "test-secret", SSLMode: "disable", Runtime: "linux"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	now := time.Now().UTC().Add(-7 * time.Minute)
	pressure := map[string]any{"version_num": 180000, "connections": 95, "max_connections": 100,
		"linux": map[string]any{"status": "ok", "memory": map[string]any{"usedPercent": 95}}}
	for _, offset := range []time.Duration{0, 301 * time.Second} {
		if _, err := pg.recordCheckAt(context.Background(), asset.ID, pressure, "", "", now.Add(offset)); err != nil {
			t.Fatal(err)
		}
	}
	var eventID string
	var eventCount int
	if err := pg.pool.QueryRow(context.Background(), `SELECT count(*)::int,min(id::text) FROM events
		WHERE asset_id=$1 AND event_group='capacity' AND kind='alert_incident'`, asset.ID).
		Scan(&eventCount, &eventID); err != nil || eventCount != 1 {
		t.Fatalf("PG and Linux alert split into events: %d %v", eventCount, err)
	}
	if err := pg.pool.QueryRow(context.Background(), `SELECT count(*)::int FROM alert_states
		WHERE asset_id=$1 AND state_json->>'eventId'=$2 AND state_json->>'status'='firing'`,
		asset.ID, eventID).Scan(&eventCount); err != nil || eventCount != 2 {
		t.Fatalf("both signals did not share event ID %s: %d %v", eventID, eventCount, err)
	}
	pressure["connections"] = 10
	if _, err := pg.recordCheckAt(context.Background(), asset.ID, pressure, "", "", now.Add(302*time.Second)); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pg.pool.QueryRow(context.Background(), `SELECT status FROM events WHERE id=$1`, eventID).Scan(&status); err != nil || status != "open" {
		t.Fatalf("event closed while Linux is still firing: %s %v", status, err)
	}
	pressure["linux"] = map[string]any{"status": "ok", "memory": map[string]any{"usedPercent": 40}}
	if _, err := pg.recordCheckAt(context.Background(), asset.ID, pressure, "", "", now.Add(303*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(context.Background(), `SELECT status FROM events WHERE id=$1`, eventID).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("grouped event did not close after both signals recovered: %s %v", status, err)
	}
}

func TestGoKubernetesOOMAndPGFailureShareEventID(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(context.Background(), assets.Input{Name: "Go Kubernetes RCA test",
		Host: "127.0.0.1", Port: 5432, Database: "postgres", Username: "postgres",
		Password: "test-secret", SSLMode: "disable", Runtime: "k8s", K8sCluster: "test",
		Namespace: "db", Pod: "pg-0"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	now := time.Now().UTC()
	k8s := map[string]any{"status": "ok", "namespace": "db", "pod": map[string]any{
		"name": "pg-0", "uid": "pod-uid", "node": "worker-1"},
		"node":     map[string]any{"name": "worker-1"},
		"services": []any{map[string]any{"name": "pg-service", "ports": []any{5432}}},
		"containers": []any{map[string]any{"name": "postgres", "terminatedReason": "OOMKilled",
			"lastTerminatedAt": now.Add(-time.Minute).Format(time.RFC3339Nano)}}}
	if _, err := pg.recordCheckAt(context.Background(), asset.ID, map[string]any{"k8s": k8s},
		"availability", "PostgreSQL connection refused", now); err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := pg.pool.QueryRow(context.Background(), `SELECT id::text FROM events
		WHERE asset_id=$1 AND event_group='availability' AND status='open'`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	detail, err := pg.EventByID(context.Background(), eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	if !strings.Contains(string(encoded), "pod_oom_correlated") ||
		!strings.Contains(string(encoded), "k8s_oom_killed") ||
		!strings.Contains(string(encoded), "pod:pod-uid") ||
		!strings.Contains(string(encoded), "routes_to") ||
		!strings.Contains(string(encoded), "supported_by") {
		t.Fatalf("PG/Kubernetes evidence missing from one event: %s", encoded)
	}
	if _, err := pg.recordCheckAt(context.Background(), asset.ID,
		map[string]any{"version_num": 180000, "connections": 1, "max_connections": 100,
			"k8s": k8s},
		"", "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pg.pool.QueryRow(context.Background(), `SELECT status FROM events WHERE id=$1`, eventID).Scan(&status); err != nil || status != "open" {
		t.Fatalf("Kubernetes incident closed while OOM signal still fires: %s %v", status, err)
	}
	k8s["containers"] = []any{}
	if _, err := pg.recordCheckAt(context.Background(), asset.ID,
		map[string]any{"version_num": 180000, "connections": 1, "max_connections": 100,
			"k8s": k8s}, "", "", now.Add(11*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(context.Background(), `SELECT status FROM events WHERE id=$1`, eventID).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("Kubernetes incident did not recover under original ID: %s %v", status, err)
	}
}
