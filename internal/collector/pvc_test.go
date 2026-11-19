package collector

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPVCUsagePrometheusScopeAndMissingMetric(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		usedQuery := `kubelet_volume_stats_used_bytes{namespace="db",persistentvolumeclaim="pg-data"}`
		capacityQuery := `kubelet_volume_stats_capacity_bytes{namespace="db",persistentvolumeclaim="pg-data"}`
		if r.Method != http.MethodGet || r.URL.Path != "/prom/api/v1/query" ||
			r.Header.Get("Authorization") != "Bearer read-only-test-token" ||
			(query != usedQuery && query != capacityQuery) {
			t.Errorf("Prometheus query was not scoped to the PVC: %s %s", r.URL.Path, r.URL.RawQuery)
		}
		value := "4096"
		if query == capacityQuery {
			value = "8192"
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"db","persistentvolumeclaim":"pg-data","instance":"node-a"},"value":[%d,%q]}]}}`,
			time.Now().Unix(), value)
	}))
	defer server.Close()
	t.Setenv("DBSPHERE_PROMETHEUS_URL", server.URL+"/prom")
	t.Setenv("DBSPHERE_PROMETHEUS_TOKEN", "read-only-test-token")
	value := PVCUsage(context.Background(), "db", "pg-data")
	if value["status"] != "ok" || value["usedBytes"] != float64(4096) ||
		value["capacityBytes"] != float64(8192) || value["usedPercent"] != float64(50) ||
		value["observedAt"] == nil {
		t.Fatalf("PVC usage metric not read: %+v", value)
	}
	value = PVCUsage(context.Background(), "../other", "pg-data")
	if value["status"] != "error" {
		t.Fatalf("invalid PVC scope accepted: %+v", value)
	}
	t.Setenv("DBSPHERE_PROMETHEUS_URL", "")
	value = PVCUsage(context.Background(), "db", "pg-data")
	if value["status"] != "not_configured" {
		t.Fatalf("unconfigured PVC metric marked healthy: %+v", value)
	}
}

func TestPVCUsageRejectsUnmatchedCapacityAndStaleSamples(t *testing.T) {
	capacityInstance := "node-b"
	stamp := time.Now().Unix()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, instance := "4096", "node-a"
		if strings.Contains(r.URL.Query().Get("query"), "capacity_bytes") {
			value, instance = "8192", capacityInstance
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"db","persistentvolumeclaim":"pg-data","instance":%q},"value":[%d,%q]}]}}`,
			instance, stamp, value)
	}))
	defer server.Close()
	t.Setenv("DBSPHERE_PROMETHEUS_URL", server.URL)
	if value := PVCUsage(context.Background(), "db", "pg-data"); value["status"] != "unavailable" {
		t.Fatalf("different kubelet targets were combined into one PVC percentage: %+v", value)
	}
	capacityInstance = "node-a"
	stamp = time.Now().Add(-10 * time.Minute).Unix()
	if value := PVCUsage(context.Background(), "db", "pg-data"); value["status"] != "unavailable" {
		t.Fatalf("stale kubelet volume stats were treated as current: %+v", value)
	}
}

func TestPVCUsageEmptyPrometheusVectorIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer server.Close()
	t.Setenv("DBSPHERE_PROMETHEUS_URL", server.URL)
	value := PVCUsage(context.Background(), "db", "pg-data")
	if value["status"] != "unavailable" || !strings.Contains(fmt.Sprint(value["reason"]), "absent") {
		t.Fatalf("missing Prometheus metric marked healthy: %+v", value)
	}
}

func TestKubernetesQuantityBytes(t *testing.T) {
	for text, expected := range map[string]float64{"5Gi": 5 * 1024 * 1024 * 1024, "500M": 500e6, "1024": 1024} {
		value, err := KubernetesQuantityBytes(text)
		if err != nil || value != expected {
			t.Fatalf("%s = %f, %v", text, value, err)
		}
	}
	if _, err := KubernetesQuantityBytes("invalid"); err == nil {
		t.Fatal("invalid quantity accepted")
	}
}
