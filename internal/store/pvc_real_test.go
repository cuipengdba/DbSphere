package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collector"
)

// This opt-in drill reads genuine kubelet volume metrics and Kubernetes Pod/PVC
// objects through kubectl. The HTTP adapter only wraps each real kubelet scrape
// in the Prometheus instant-vector shape expected by the production collector.
func TestRealKubernetesPVCPressureOpensAndRecoversOneIncident(t *testing.T) {
	if os.Getenv("DBSPHERE_TEST_REAL_PVC") != "1" {
		t.Skip("requires a dedicated Kubernetes CSI PVC and PostgreSQL 18 target")
	}
	storeURL, kubeconfig := os.Getenv("DBSPHERE_TEST_PG18_URL"), os.Getenv("DBSPHERE_TEST_KUBECONFIG")
	namespace, pvcName := os.Getenv("DBSPHERE_TEST_PVC_NAMESPACE"), os.Getenv("DBSPHERE_TEST_PVC_NAME")
	podName, nodeName := os.Getenv("DBSPHERE_TEST_PVC_POD"), os.Getenv("DBSPHERE_TEST_KUBE_NODE")
	port, err := strconv.Atoi(os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT"))
	password := os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")
	if storeURL == "" || kubeconfig == "" || namespace == "" || pvcName == "" || podName == "" ||
		nodeName == "" || err != nil || password == "" {
		t.Fatal("real PVC drill needs PG18 store/target and Kubernetes test scope")
	}
	ctx := context.Background()
	kubectl := func(args ...string) ([]byte, error) {
		commandCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		command := exec.CommandContext(commandCtx, "kubectl", append([]string{"--kubeconfig", kubeconfig}, args...)...)
		return command.CombinedOutput()
	}
	readMetric := func(raw []byte, name string) (float64, bool) {
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(line, name+"{") ||
				!strings.Contains(line, `namespace="`+namespace+`"`) ||
				!strings.Contains(line, `persistentvolumeclaim="`+pvcName+`"`) {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			value, err := strconv.ParseFloat(fields[len(fields)-1], 64)
			return value, err == nil
		}
		return 0, false
	}
	var current map[string]float64
	var metricLock sync.RWMutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		metric := strings.SplitN(r.URL.Query().Get("query"), "{", 2)[0]
		metricLock.RLock()
		value, ok := current[metric]
		metricLock.RUnlock()
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/query" || !ok {
			http.Error(w, "metric unavailable", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "success", "data": map[string]any{
			"resultType": "vector", "result": []any{map[string]any{
				"metric": map[string]string{"namespace": namespace, "persistentvolumeclaim": pvcName, "instance": nodeName},
				"value":  []any{float64(time.Now().Unix()), strconv.FormatFloat(value, 'f', -1, 64)},
			}},
		}})
	}))
	defer server.Close()
	t.Setenv("DBSPHERE_PROMETHEUS_URL", server.URL)
	refresh := func() error {
		raw, err := kubectl("get", "--raw", "/api/v1/nodes/"+nodeName+"/proxy/metrics", "--request-timeout=20s")
		if err != nil {
			return fmt.Errorf("read real kubelet metrics: %w", err)
		}
		values := map[string]float64{}
		for _, name := range []string{"kubelet_volume_stats_used_bytes", "kubelet_volume_stats_capacity_bytes"} {
			value, ok := readMetric(raw, name)
			if !ok {
				return fmt.Errorf("real kubelet metric %s is absent", name)
			}
			values[name] = value
		}
		metricLock.Lock()
		current = values
		metricLock.Unlock()
		return nil
	}
	readK8S := func() (map[string]any, error) {
		podRaw, err := kubectl("get", "pod", podName, "-n", namespace, "-o", "json", "--request-timeout=20s")
		if err != nil {
			return nil, fmt.Errorf("read real PostgreSQL Pod: %w", err)
		}
		pvcRaw, err := kubectl("get", "pvc", pvcName, "-n", namespace, "-o", "json", "--request-timeout=20s")
		if err != nil {
			return nil, fmt.Errorf("read real PostgreSQL PVC: %w", err)
		}
		var pod, pvc map[string]any
		if json.Unmarshal(podRaw, &pod) != nil || json.Unmarshal(pvcRaw, &pvc) != nil {
			return nil, fmt.Errorf("Kubernetes Pod or PVC response is invalid")
		}
		spec, state := object(pod["spec"]), object(pod["status"])
		bound := false
		for _, item := range list(spec["volumes"]) {
			bound = bound || stringValue(object(object(item)["persistentVolumeClaim"])["claimName"]) == pvcName
		}
		if !bound || stringValue(state["phase"]) != "Running" ||
			stringValue(object(pvc["status"])["phase"]) != "Bound" {
			return nil, fmt.Errorf("PostgreSQL Pod is not running on the bound test PVC")
		}
		usage := collector.PVCUsage(ctx, namespace, pvcName)
		if usage["status"] != "ok" {
			return nil, fmt.Errorf("real PVC usage unavailable: %v", usage)
		}
		return map[string]any{"status": "ok", "namespace": namespace,
			"pod": map[string]any{"name": podName, "uid": object(pod["metadata"])["uid"],
				"phase": state["phase"], "node": spec["nodeName"], "conditions": state["conditions"]},
			"pvcs": []any{map[string]any{"name": pvcName, "namespace": namespace,
				"phase":    object(pvc["status"])["phase"],
				"capacity": object(object(pvc["status"])["capacity"])["storage"], "usage": usage}}}, nil
	}
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Real CSI PVC pressure drill",
		Host: "127.0.0.1", Port: port, Database: "postgres", Username: "postgres",
		Password: password, SSLMode: "disable", Runtime: "k8s", K8sCluster: "kind-dbsphere-test",
		Namespace: namespace, Pod: podName}, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(ctx, asset.ID)
	zero, original := 0, 0
	if err := pg.pool.QueryRow(ctx, `SELECT (definition_json->>'durationSeconds')::int
		FROM alert_rules WHERE id='k8s_pvc_high'`).Scan(&original); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.UpdateAlertRule(ctx, "k8s_pvc_high", AlertRulePatch{DurationSeconds: &zero}); err != nil {
		t.Fatal(err)
	}
	defer pg.UpdateAlertRule(context.Background(), "k8s_pvc_high", AlertRulePatch{DurationSeconds: &original})
	defer kubectl("exec", "-n", namespace, podName, "--", "rm", "-f", "/data/dbsphere-fill")
	if err := refresh(); err != nil {
		t.Fatal(err)
	}
	metricLock.RLock()
	baselineUsed := current["kubelet_volume_stats_used_bytes"]
	capacity := current["kubelet_volume_stats_capacity_bytes"]
	metricLock.RUnlock()
	if capacity <= 0 || baselineUsed/capacity >= 0.8 {
		t.Fatalf("test PVC must start below 80%% usage: %.0f/%.0f", baselineUsed, capacity)
	}
	fillMiB := int(math.Ceil((0.88*capacity - baselineUsed) / (1024 * 1024)))
	if fillMiB < 1 || fillMiB > 24 {
		t.Fatalf("bounded test PVC filler would need %d MiB", fillMiB)
	}
	if _, err := kubectl("exec", "-n", namespace, podName, "--", "dd", "if=/dev/zero",
		"of=/data/dbsphere-fill", "bs=1M", "count="+strconv.Itoa(fillMiB), "conv=fsync"); err != nil {
		t.Fatal("write bounded test PVC filler", err)
	}
	waitUsage := func(high bool) (map[string]any, error) {
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			if err := refresh(); err == nil {
				k8s, err := readK8S()
				if err == nil {
					percent := number(object(object(list(k8s["pvcs"])[0])["usage"])["usedPercent"])
					if (high && percent >= 85) || (!high && percent < 85) {
						return k8s, nil
					}
				}
			}
			time.Sleep(5 * time.Second)
		}
		return nil, fmt.Errorf("kubelet PVC usage did not become high=%t", high)
	}
	record := func(k8s map[string]any) CheckResult {
		t.Helper()
		metrics, err := collector.PostgreSQL(ctx, collector.Target{Host: "127.0.0.1", Port: port,
			Database: "postgres", Username: "postgres", SSLMode: "disable"}, password)
		if err != nil {
			t.Fatal("real Kubernetes PostgreSQL collection", err)
		}
		metrics["collector"] = map[string]any{"pgStatus": "ok"}
		metrics["k8s"] = k8s
		result, err := pg.recordCheck(ctx, asset.ID, metrics, "", "")
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	high, err := waitUsage(true)
	if err != nil {
		t.Fatal(err)
	}
	fault := record(high)
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1
		AND event_group='storage' AND status='open'`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal("real PVC usage did not open an incident", err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	if !strings.Contains(string(encoded), `"code":"pvc_pressure"`) ||
		!strings.Contains(string(encoded), fault.SampleID) || !strings.Contains(string(encoded), pvcName) {
		t.Fatal("real PVC incident lacks sample, PVC and candidate evidence")
	}
	if _, err := kubectl("exec", "-n", namespace, podName, "--", "rm", "-f", "/data/dbsphere-fill"); err != nil {
		t.Fatal("remove test PVC filler", err)
	}
	healthy, err := waitUsage(false)
	if err != nil {
		t.Fatal(err)
	}
	recovery := record(healthy)
	var status string
	if err := pg.pool.QueryRow(ctx, `SELECT status FROM events WHERE id=$1`, eventID).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("real PVC event did not recover under one ID: %s %v", status, err)
	}
	if report, err := pg.EventReport(ctx, eventID, "md"); err != nil ||
		!strings.Contains(report, fault.SampleID) || !strings.Contains(report, recovery.SampleID) {
		t.Fatal("real PVC incident report lost fault or recovery sample", err)
	}
	t.Logf("real CSI PVC event %s: fault %s, recovery %s", eventID, fault.SampleID, recovery.SampleID)
}
