package store

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRCAPolicyReplayEvidenceMatrix(t *testing.T) {
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name, code, metric string
		metrics            map[string]any
		value              float64
	}{
		{"lock", "lock_chain", "pg.blocked_sessions", map[string]any{"diagnostics": map[string]any{
			"lockChain": map[string]any{"status": "ok", "data": []any{map[string]any{"waitingPid": 20, "blockerPid": 10}}}}}, 1},
		{"proxy", "route_backend_mismatch", "routing.entry_fault", map[string]any{
			"routing": map[string]any{"status": "misrouted", "entry": "proxy:5432"}}, 1},
		{"replication", "replication_lag", "pg.replication_lag_bytes", map[string]any{
			"replication_lag_bytes": float64(128 * 1024 * 1024), "role": "primary"}, 128 * 1024 * 1024},
		{"disk", "linux_disk_pressure", "linux.disk_used_percent", map[string]any{
			"linux": map[string]any{"status": "ok"}}, 96},
		{"kubernetes", "k8s_pod_not_ready", "k8s.pod_not_ready", map[string]any{
			"k8s": map[string]any{"status": "ok", "pod": map[string]any{"name": "db-1", "conditions": []any{
				map[string]any{"type": "Ready", "status": "False"}}}}}, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload, _ := json.Marshal(map[string]any{"metric": test.metric, "value": test.value,
				"sampleId": "sample", "threshold": 1, "operator": ">="})
			event := &eventRecord{ID: "event", StartedAt: at,
				Evidence: []eventEvidence{{ID: "alert", Kind: "alert_firing", Payload: payload, ObservedAt: at}}}
			samples := map[string]sampleObservation{"sample": {ID: "sample", At: at, Status: "ok", Metrics: test.metrics}}
			candidates := analyzeCandidates(event, samples, func(string, string, string, string, any) {}, &[]map[string]string{})
			if len(candidates) != 1 || candidates[0]["code"] != test.code {
				t.Fatalf("source sample did not generate the expected candidate: %+v", candidates)
			}
			candidate := candidates[0]
			active := rcaPolicy{Code: test.code, Enabled: true, Version: 1, MinEvidence: 2, RequireOnset: true}
			result := evaluateRCACandidate(candidate, active, at)
			if result.Outcome != "matched" || result.Candidate["coverage"].(map[string]int)["supports"] != 2 {
				t.Fatalf("rule should preserve two raw references: %+v", result)
			}
			active.MinEvidence = 3
			if result := evaluateRCACandidate(candidate, active, at); result.Outcome != "evidence_below_minimum" {
				t.Fatalf("one extra reference should be required: %+v", result)
			}
			if result := evaluateRCACandidate(nil, active, at); result.Outcome != "no_candidate" {
				t.Fatalf("time overlap alone produced a candidate: %+v", result)
			}
		})
	}
	plan := map[string]any{"code": "sql_plan_regression", "supports": []string{"sample:baseline", "plan:before", "plan:after", "plan:after"},
		"against": []string{"另有同期 CPU I/O 影响"}, "missing": []string{"复核计划参数"},
		"timeOrder": map[string]any{"firstObservedAt": at.Add(time.Second), "relation": "after_event"}}
	planPolicy := rcaPolicy{Code: "sql_plan_regression", Enabled: true, Version: 1, MinEvidence: 3, RequireOnset: true}
	result := evaluateRCACandidate(plan, planPolicy, at)
	if result.Outcome != "onset_not_established" || result.Candidate["coverage"].(map[string]int)["supports"] != 3 ||
		result.Candidate["coverage"].(map[string]int)["against"] != 1 {
		t.Fatalf("plan replay lost timing, deduplication or counterevidence: %+v", result)
	}
	planPolicy.RequireOnset = false
	if result := evaluateRCACandidate(plan, planPolicy, at); result.Outcome != "matched" {
		t.Fatalf("explicitly allowing later evidence should preserve the plan candidate: %+v", result)
	}
}

func TestRealPG18RCARuleRevisionReplaysRecoveredEvent(t *testing.T) {
	healthy := os.Getenv("DBSPHERE_TEST_HAPROXY_RECOVERY")
	if healthy == "" {
		t.Skip("a working PostgreSQL proxy entry is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pg, asset, key := routeTestAsset(t, ctx, "")
	set := func(entry string) {
		t.Helper()
		if _, err := pg.SetRouting(ctx, asset, RoutingInput{RoutingSettings: RoutingSettings{
			Kind: "haproxy", Entry: entry, ExpectedRole: "primary"}}, key); err != nil {
			t.Fatal(err)
		}
	}
	unreachable := func() string {
		t.Helper()
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		return listener.Addr().String()
	}
	set(unreachable())
	fault, _ := checkTestRoute(t, ctx, pg, asset, key, "unreachable")
	code := "route_entry_unreachable"
	event := checkRouteIncident(t, ctx, pg, asset, code, fault)
	if err := pg.RecordRCADecision(ctx, event, code, "confirmed", "监控端口不可连接"); err != nil {
		t.Fatal(err)
	}
	set(healthy)
	recovered, _ := checkTestRoute(t, ctx, pg, asset, key, "ok")
	checkRouteRecovery(t, ctx, pg, event, recovered)
	enabled, onset, count := true, false, 8
	config := RCARuleInput{ExpectedVersion: 1, Enabled: &enabled, MinEvidence: &count, RequireOnset: &onset, Reason: "提高证据要求"}
	update, err := pg.UpdateRCARule(ctx, code, config)
	if err != nil || update.(rcaPolicy).Version != 2 {
		t.Fatalf("expected a new version: %+v %v", update, err)
	}
	if _, err := pg.UpdateRCARule(ctx, code, config); !errors.Is(err, ErrRCARuleConflict) {
		t.Fatal("stale rule editor version was accepted", err)
	}
	replay, err := pg.ReplayRCARule(ctx, event, code)
	if err != nil {
		t.Fatal(err)
	}
	view := testRouteJSON(t, replay)
	historical, current := object(view["historical"]), object(view["current"])
	if view["eventId"] != event || view["eventStatus"] != "resolved" || historical["version"] != float64(1) ||
		historical["outcome"] != "matched" || current["version"] != float64(2) || current["outcome"] != "evidence_below_minimum" ||
		object(object(historical["candidate"])["review"])["decision"] != "confirmed" {
		t.Fatalf("historical rule, current rule or review changed unexpectedly: %+v", view)
	}
	report, err := pg.EventReport(ctx, event, "md")
	if err != nil || !strings.Contains(report, "规则 v1") || !strings.Contains(report, "监控端口不可连接") {
		t.Fatalf("recovery report lost the original rule or approval: %v", err)
	}
	set(unreachable())
	newSample, _ := checkTestRoute(t, ctx, pg, asset, key, "unreachable")
	var newEvent string
	if err := pg.pool.QueryRow(ctx, "SELECT id::text FROM events WHERE asset_id=$1 AND status='open'", asset).Scan(&newEvent); err != nil {
		t.Fatal(err)
	}
	newDetail, err := pg.EventByID(ctx, newEvent)
	if err != nil {
		t.Fatal(err)
	}
	data := testRouteJSON(t, newDetail)
	analysis := object(data["analysis"])
	if newEvent == event || len(list(analysis["candidates"])) != 0 || analysis["status"] != "unlocated" {
		t.Fatalf("new event did not use the stricter policy: %+v", analysis)
	}
	for _, node := range list(object(analysis["graph"])["nodes"]) {
		if object(node)["id"] == "candidate:"+code {
			t.Fatal("filtered candidate persisted in the graph")
		}
	}
	if raw, err := pg.RawSample(ctx, asset, newSample); err != nil || raw == nil {
		t.Fatal("filtered candidate discarded its original sample", err)
	}
	t.Logf("recovered=%s baseline=v1 current=v2 newEvent=%s sample=%s", event, newEvent, newSample)
}
