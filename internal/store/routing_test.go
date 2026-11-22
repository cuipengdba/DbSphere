package store

import (
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/collector"
)

func TestRouteAssessmentDistinguishesProxyAndServiceEvidence(t *testing.T) {
	now := time.Now()
	direct := map[string]any{"postgresStartedAt": now, "databaseName": "postgres",
		"postgresEndpoint": map[string]any{"address": "192.0.2.1", "port": 5432}, "role": "primary"}
	moved := collector.RouteProbe{Status: "ok", Backend: "192.0.2.2:5432", StartedAt: now.Add(time.Minute),
		Role: "primary", Database: "postgres"}
	comparison := RoutingSettings{Kind: "haproxy", Entry: "proxy.local:5432", ExpectedRole: "primary", MatchDirect: true}
	result := routeAssessment(comparison, moved, direct, nil, nil, nil, "monitor")
	if result["status"] != "misrouted" || result["differentBackend"] != true || result["roleMismatch"] != false {
		t.Fatalf("different primary was not identified: %+v", result)
	}
	moved.StartedAt = now
	result = routeAssessment(comparison, moved, direct, nil, nil, nil, "monitor")
	if result["status"] != "ok" || result["differentBackend"] != false {
		t.Fatalf("another address alone must not imply different backend: %+v", result)
	}
	kube := map[string]any{"status": "ok", "namespace": "data",
		"pod": map[string]any{"uid": "pod-1", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}},
		"services": []any{map[string]any{"name": "db", "uid": "service-1", "ports": []any{6432},
			"portMappings": []any{map[string]any{"port": 6432, "targetPort": 6433, "protocol": "TCP"}}}},
		"endpointRoutes": map[string]any{"slices": map[string]any{"main": map[string]any{"service": "db", "uid": "slice-1"}}}}
	comparison = RoutingSettings{Kind: "k8s_service", Entry: "db.data.svc.cluster.local:6432"}
	failed := collector.RouteProbe{Status: "unreachable"}
	result = routeAssessment(comparison, failed, direct, kube, nil, nil, "monitor")
	service := object(result["service"])
	if result["status"] != "ready_pod_entry_unreachable" || service["podUid"] != "pod-1" ||
		object(object(service["slices"])["main"])["uid"] != "slice-1" {
		t.Fatalf("verified Service and Pod evidence missing: %+v", result)
	}
	comparison.Entry = "unrelated.data.svc.cluster.local:6432"
	result = routeAssessment(comparison, failed, direct, kube, nil, nil, "monitor")
	if result["status"] != "unreachable" || result["service"] != nil {
		t.Fatalf("unrelated Service produced a mapped diagnosis: %+v", result)
	}
	pooler := map[string]any{"status": "ok", "pools": []map[string]any{
		{"user": "monitor", "clientWaiting": 1, "serverActive": 1, "serverIdle": 0},
	}}
	comparison = RoutingSettings{Kind: "pgbouncer", Entry: "pooler.local:6432"}
	result = routeAssessment(comparison, collector.RouteProbe{Status: "timeout"}, direct, nil, pooler, nil, "monitor")
	if result["status"] != "pool_exhausted" {
		t.Fatalf("pool queue not recognized: %+v", result)
	}
}
