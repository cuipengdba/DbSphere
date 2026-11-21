package collector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeKubeConfig(t *testing.T, server, ca, user string) {
	t.Helper()
	config := fmt.Sprintf(`apiVersion: v1
clusters:
- name: test-cluster
  cluster:
    server: %q
    certificate-authority-data: %q
contexts:
- name: test-context
  context: {cluster: test-cluster, user: reader}
users:
- name: reader
  user: {%s}
current-context: test-context
`, server, base64.StdEncoding.EncodeToString([]byte(ca)), user)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBSPHERE_KUBECONFIG", path)
}

func TestVerifyKubernetesApplicationPodUsesOneReadOnlyUIDCheckedRequest(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer read-only-token" {
			t.Errorf("unexpected application Pod lookup: %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/v1/namespaces/apps/pods/checkout-0":
			fmt.Fprint(w, `{"metadata":{"namespace":"apps","name":"checkout-0","uid":"pod-uid-1","creationTimestamp":"2026-11-19T00:00:00Z"},`+
				`"spec":{"nodeName":"worker-1","volumes":[{"persistentVolumeClaim":{"claimName":"checkout-data"}},`+
				`{"persistentVolumeClaim":{"claimName":"checkout-data"}}]}}`)
		case "/api/v1/namespaces/apps/persistentvolumeclaims/checkout-data":
			fmt.Fprint(w, `{"metadata":{"namespace":"apps","name":"checkout-data"},"status":{"phase":"Bound"},`+
				`"spec":{"volumeName":"pv-checkout"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	writeKubeConfig(t, server.URL, ca, `token: read-only-token`)
	target := KubernetesTarget{Cluster: "test-context", Namespace: "apps", Pod: "checkout-0"}
	got, err := VerifyKubernetesPodIdentity(context.Background(), target, "pod-uid-1")
	if err != nil || got.UID != "pod-uid-1" || got.Node != "worker-1" ||
		len(got.PVCs) != 1 || got.PVCs[0] != "checkout-data" ||
		len(got.PVCStates) != 1 || got.PVCStates[0].Status != "ok" ||
		got.PVCStates[0].Phase != "Bound" || got.PVCStates[0].VolumeName != "pv-checkout" {
		t.Fatalf("application Pod identity was not verified: %+v %v", got, err)
	}
	if _, err := VerifyKubernetesPodIdentity(context.Background(), target, "replaced-uid"); err == nil {
		t.Fatal("recreated Pod name was accepted without matching UID")
	}
	if _, err := VerifyKubernetesPodIdentity(context.Background(), KubernetesTarget{
		Cluster: "test-context", Namespace: "../other", Pod: "checkout-0"}, "pod-uid-1"); err == nil {
		t.Fatal("invalid namespace was sent to Kubernetes API")
	}
	if requests != 3 {
		t.Fatalf("expected three scoped GET requests, got %d", requests)
	}
}

func TestVerifyKubernetesApplicationPodKeepsPVCStatusUnknownWhenDenied(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/namespaces/apps/pods/checkout-0":
			fmt.Fprint(w, `{"metadata":{"namespace":"apps","name":"checkout-0","uid":"pod-uid-1",`+
				`"creationTimestamp":"2026-11-19T00:00:00Z"},"spec":{"volumes":[`+
				`{"persistentVolumeClaim":{"claimName":"checkout-data"}}]}}`)
		case "/api/v1/namespaces/apps/persistentvolumeclaims/checkout-data":
			http.Error(w, "forbidden", http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	writeKubeConfig(t, server.URL, ca, `token: read-only-token`)
	got, err := VerifyKubernetesPodIdentity(context.Background(), KubernetesTarget{
		Cluster: "test-context", Namespace: "apps", Pod: "checkout-0"}, "pod-uid-1")
	if err != nil || got.UID != "pod-uid-1" || len(got.PVCs) != 1 ||
		len(got.PVCStates) != 1 || got.PVCStates[0].Status != "unavailable" ||
		got.PVCStates[0].Phase != "" || got.PVCStates[0].VolumeName != "" {
		t.Fatalf("PVC read denial should not invalidate Pod identity or invent a phase: %+v %v", got, err)
	}
}

func TestRealKubernetesApplicationPodIdentity(t *testing.T) {
	namespace := os.Getenv("DBSPHERE_TEST_K8S_APP_NAMESPACE")
	name := os.Getenv("DBSPHERE_TEST_K8S_APP_POD")
	uid := os.Getenv("DBSPHERE_TEST_K8S_APP_UID")
	if namespace == "" || name == "" || uid == "" ||
		(os.Getenv("DBSPHERE_KUBECONFIG") == "" && os.Getenv("KUBERNETES_SERVICE_HOST") == "") {
		t.Skip("real read-only Kubernetes Pod identity test is not configured")
	}
	got, err := VerifyKubernetesPodIdentity(context.Background(), KubernetesTarget{
		Cluster: os.Getenv("DBSPHERE_TEST_K8S_CONTEXT"), Namespace: namespace, Pod: name}, uid)
	if err != nil || got.Namespace != namespace || got.Name != name || got.UID != uid ||
		got.CreatedAt.IsZero() {
		t.Fatalf("real application Pod identity was not verified: %+v %v", got, err)
	}
	if expectedNode := os.Getenv("DBSPHERE_TEST_K8S_APP_NODE"); expectedNode != "" && got.Node != expectedNode {
		t.Fatalf("real application Pod was scheduled on %q, expected %q", got.Node, expectedNode)
	}
	if expectedPVC := os.Getenv("DBSPHERE_TEST_K8S_APP_PVC"); expectedPVC != "" {
		if len(got.PVCs) != 1 || got.PVCs[0] != expectedPVC || len(got.PVCStates) != 1 ||
			got.PVCStates[0].Name != expectedPVC || got.PVCStates[0].Status != "ok" ||
			got.PVCStates[0].Phase != os.Getenv("DBSPHERE_TEST_K8S_APP_PVC_PHASE") {
			t.Fatalf("real application Pod PVC state was not verified: %+v", got)
		}
	}
	if _, err := VerifyKubernetesPodIdentity(context.Background(), KubernetesTarget{
		Cluster: os.Getenv("DBSPHERE_TEST_K8S_CONTEXT"), Namespace: namespace, Pod: name}, "00000000-0000-4000-8000-000000000000"); err == nil {
		t.Fatal("real Kubernetes API accepted a mismatched Pod UID")
	}
}

func TestKubernetesReadOnlyMappedPodEvidence(t *testing.T) {
	paths := []string{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer read-only-token" {
			t.Errorf("Kubernetes request was not read-only or authenticated: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/db/pods/pg-0":
			fmt.Fprint(w, `{"metadata":{"name":"pg-0","uid":"pod-uid","labels":{"app":"postgres"}},"spec":{"nodeName":"worker-1","volumes":[{"persistentVolumeClaim":{"claimName":"pg-data"}}]},"status":{"phase":"Running","containerStatuses":[{"name":"postgres","restartCount":1,"lastState":{"terminated":{"reason":"OOMKilled","finishedAt":"2026-11-17T00:00:00Z"}}}]}}`)
		case "/api/v1/nodes/worker-1":
			fmt.Fprint(w, `{"status":{"conditions":[{"type":"Ready","status":"True"}],"allocatable":{"memory":"2Gi"}}}`)
		case "/api/v1/namespaces/db/persistentvolumeclaims/pg-data":
			fmt.Fprint(w, `{"status":{"phase":"Bound","capacity":{"storage":"5Gi"}},"spec":{"volumeName":"pv-1"}}`)
		case "/api/v1/persistentvolumes/pv-1":
			fmt.Fprint(w, `{"status":{"phase":"Bound"},"spec":{"capacity":{"storage":"5Gi"}}}`)
		case "/api/v1/namespaces/db/events":
			if r.URL.Query().Get("fieldSelector") != "involvedObject.uid=pod-uid" {
				t.Errorf("events were not scoped by Pod UID: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"items":[{"reason":"Unhealthy","message":"readiness probe failed","lastTimestamp":"2026-11-17T00:01:00Z"}]}`)
		case "/api/v1/namespaces/db/services":
			if r.URL.Query().Get("limit") != "100" {
				t.Errorf("Service list was not bounded: %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"pg-service"},"spec":{"selector":{"app":"postgres"},"ports":[{"port":5432}]}},{"metadata":{"name":"other"},"spec":{"selector":{"app":"other"}}}]}`)
		case "/apis/metrics.k8s.io/v1beta1/namespaces/db/pods/pg-0":
			fmt.Fprint(w, `{"timestamp":"2026-11-17T00:01:00Z","containers":[{"name":"postgres","usage":{"cpu":"20m","memory":"128Mi"}}]}`)
		case "/apis/metrics.k8s.io/v1beta1/nodes/worker-1":
			fmt.Fprint(w, `{"timestamp":"2026-11-17T00:01:00Z","usage":{"cpu":"150m","memory":"512Mi"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	writeKubeConfig(t, server.URL, ca, `token: read-only-token`)
	prometheus := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query().Get("query")
		usedQuery := `kubelet_volume_stats_used_bytes{namespace="db",persistentvolumeclaim="pg-data"}`
		capacityQuery := `kubelet_volume_stats_capacity_bytes{namespace="db",persistentvolumeclaim="pg-data"}`
		if query != usedQuery && query != capacityQuery {
			t.Errorf("PVC Prometheus query was not scoped: %s", r.URL.RawQuery)
		}
		value := "4294967296"
		if query == capacityQuery {
			value = "8589934592"
		}
		fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"namespace":"db","persistentvolumeclaim":"pg-data","instance":"node-a"},"value":[%d,%q]}]}}`,
			time.Now().Unix(), value)
	}))
	defer prometheus.Close()
	t.Setenv("DBSPHERE_PROMETHEUS_URL", prometheus.URL)
	value := Kubernetes(context.Background(), KubernetesTarget{Cluster: "test-context", Namespace: "db", Pod: "pg-0"})
	if value["status"] != "ok" || kubeObject(value["pod"])["uid"] != "pod-uid" ||
		kubeObject(value["node"])["name"] != "worker-1" || len(kubeList(value["pvcs"])) != 1 ||
		len(kubeList(value["events"])) != 1 || len(kubeList(value["containers"])) != 1 {
		t.Fatalf("mapped Kubernetes evidence is incomplete: %+v", value)
	}
	container := kubeObject(kubeList(value["containers"])[0])
	if container["terminatedReason"] != "OOMKilled" {
		t.Fatalf("OOM evidence missing: %+v", container)
	}
	if kubeObject(kubeList(value["pvcs"])[0])["volume"] == nil {
		t.Fatal("bound PersistentVolume was not linked to its PVC")
	}
	if usage := kubeObject(kubeObject(kubeList(value["pvcs"])[0])["usage"]); usage["status"] != "ok" ||
		usage["usedBytes"] != float64(4294967296) || usage["capacityBytes"] != float64(8589934592) {
		t.Fatalf("Prometheus PVC usage was not attached to its claim: %+v", usage)
	}
	if services := kubeList(value["services"]); len(services) != 1 || kubeObject(services[0])["name"] != "pg-service" {
		t.Fatalf("Pod labels did not select the matching Service: %+v", services)
	}
	resources := kubeObject(value["resourceMetrics"])
	usageRows := kubeList(resources["containers"])
	if resources["source"] != "metrics.k8s.io" || len(usageRows) != 1 ||
		kubeObject(resources["node"])["memory"] != "512Mi" || kubeObject(resources["podUsage"])["cpuCores"] != 0.02 ||
		kubeObject(resources["podUsage"])["memoryBytes"] != float64(128*1024*1024) || kubeObject(usageRows[0])["cpuCores"] != 0.02 {
		t.Fatalf("Pod/Node resource metrics are missing: %+v", resources)
	}
	if len(paths) != 8 {
		t.Fatalf("unexpected Kubernetes API calls: %v", paths)
	}
}

func TestKubernetesRejectsDynamicCredentialsAndInvalidScope(t *testing.T) {
	writeKubeConfig(t, "https://k8s.example.invalid", "", `exec: {command: /bin/sh}`)
	value := Kubernetes(context.Background(), KubernetesTarget{Cluster: "test-context", Namespace: "db", Pod: "pg-0"})
	if value["status"] != "not_configured" || !strings.Contains(fmt.Sprint(value["reason"]), "credential helpers are forbidden") {
		t.Fatalf("exec credential plugin accepted: %+v", value)
	}
	value = Kubernetes(context.Background(), KubernetesTarget{Cluster: "test-context", Namespace: "../other", Pod: "pg-0"})
	if value["status"] != "error" {
		t.Fatalf("invalid namespace accepted: %+v", value)
	}
}

func TestKubernetesMetricsPermissionGapDoesNotHidePodStatus(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/namespaces/db/pods/pg-0" {
			fmt.Fprint(w, `{"metadata":{"name":"pg-0","uid":"pod-uid"},"spec":{"nodeName":"worker-1"},"status":{"phase":"Running","containerStatuses":[]}}`)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/apis/metrics.k8s.io/") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	writeKubeConfig(t, server.URL, ca, `token: read-only-token`)
	value := Kubernetes(context.Background(), KubernetesTarget{Cluster: "test-context", Namespace: "db", Pod: "pg-0"})
	if value["status"] != "ok" || kubeObject(value["pod"])["name"] != "pg-0" ||
		value["resourceMetrics"] != nil || !strings.Contains(fmt.Sprint(value["resourceMetricsError"]), "403") ||
		!strings.Contains(fmt.Sprint(value["nodeMetricsError"]), "403") {
		t.Fatalf("metrics permission gap hid Pod health or fabricated CPU/memory: %+v", value)
	}
}

func TestKubernetesWorkloadFollowsUniqueReplacementAndRejectsAmbiguity(t *testing.T) {
	secondReady := false
	singleUnready := false
	newMetricsRead := false
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer read-only-token" {
			t.Errorf("Workload discovery must be authenticated and read-only: %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/db/pods/pg-old":
			http.NotFound(w, r)
		case "/apis/apps/v1/namespaces/db/deployments":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"postgres","uid":"deployment-uid"},"spec":{"selector":{"matchLabels":{"app":"postgres"}}}}]}`)
		case "/apis/apps/v1/namespaces/db/statefulsets":
			fmt.Fprint(w, `{"items":[]}`)
		case "/apis/apps/v1/namespaces/db/replicasets":
			fmt.Fprint(w, `{"items":[{"metadata":{"uid":"rs-uid","ownerReferences":[{"kind":"Deployment","uid":"deployment-uid"}]}}]}`)
		case "/api/v1/namespaces/db/pods":
			if r.URL.Query().Get("limit") != "100" {
				t.Errorf("Pod discovery is not bounded: %s", r.URL.RawQuery)
			}
			if singleUnready {
				fmt.Fprint(w, `{"items":[{"metadata":{"name":"pg-new","labels":{"app":"postgres"},"ownerReferences":[{"kind":"ReplicaSet","uid":"rs-uid"}]},"status":{"conditions":[{"type":"Ready","status":"False"}]}}]}`)
				return
			}
			other := "False"
			if secondReady {
				other = "True"
			}
			fmt.Fprintf(w, `{"items":[{"metadata":{"name":"pg-new","labels":{"app":"postgres"},"ownerReferences":[{"kind":"ReplicaSet","uid":"rs-uid"}]},"status":{"conditions":[{"type":"Ready","status":"True"}]}},{"metadata":{"name":"pg-other","labels":{"app":"postgres"},"ownerReferences":[{"kind":"ReplicaSet","uid":"rs-uid"}]},"status":{"conditions":[{"type":"Ready","status":%q}]}},{"metadata":{"name":"unrelated","labels":{"app":"postgres"},"ownerReferences":[{"kind":"ReplicaSet","uid":"unrelated-rs"}]},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`, other)
		case "/api/v1/namespaces/db/pods/pg-new":
			if singleUnready {
				fmt.Fprint(w, `{"metadata":{"name":"pg-new","uid":"new-uid","labels":{"app":"postgres"}},"spec":{},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"False"}],"containerStatuses":[{"name":"postgres","ready":false,"restartCount":1,"lastState":{"terminated":{"reason":"OOMKilled","finishedAt":"2026-11-18T00:00:00Z"}}}]}}`)
				return
			}
			fmt.Fprint(w, `{"metadata":{"name":"pg-new","uid":"new-uid","labels":{"app":"postgres"}},"spec":{},"status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}`)
		case "/api/v1/namespaces/db/services", "/api/v1/namespaces/db/events":
			fmt.Fprint(w, `{"items":[]}`)
		case "/apis/metrics.k8s.io/v1beta1/namespaces/db/pods/pg-new":
			newMetricsRead = true
			fmt.Fprint(w, `{"containers":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	writeKubeConfig(t, server.URL, ca, `token: read-only-token`)
	target := KubernetesTarget{Cluster: "test-context", Namespace: "db", Workload: "postgres", Pod: "pg-old"}
	value := Kubernetes(context.Background(), target)
	if value["status"] != "ok" || kubeObject(value["pod"])["name"] != "pg-new" ||
		kubeObject(value["podMapping"])["resolved"] != "pg-new" || !newMetricsRead {
		t.Fatalf("replacement Pod was not selected consistently: %+v", value)
	}
	target.Pod = ""
	if value = Kubernetes(context.Background(), target); value["status"] != "ok" ||
		kubeObject(value["pod"])["name"] != "pg-new" {
		t.Fatalf("workload-only Pod mapping failed: %+v", value)
	}
	target.Pod = "pg-old"
	singleUnready = true
	value = Kubernetes(context.Background(), target)
	if value["status"] != "ok" || kubeObject(value["podMapping"])["resolved"] != "pg-new" ||
		kubeString(kubeObject(kubeList(value["containers"])[0])["terminatedReason"]) != "OOMKilled" {
		t.Fatalf("unique unready replacement hid OOM evidence: %+v", value)
	}
	singleUnready = false
	secondReady = true
	value = Kubernetes(context.Background(), target)
	if value["status"] != "error" || !strings.Contains(fmt.Sprint(value["reason"]), "ambiguous") {
		t.Fatalf("multiple ready Pods were silently mapped: %+v", value)
	}
}

func TestKubernetesWorkloadDiscoveryFollowsBoundedPages(t *testing.T) {
	pageCalls := map[string]int{}
	writeList := func(w http.ResponseWriter, items []any, next string) {
		data, err := json.Marshal(map[string]any{"metadata": map[string]any{"continue": next}, "items": items})
		if err != nil {
			t.Error(err)
			return
		}
		_, _ = w.Write(data)
	}
	dummies := func(kind string) []any {
		items := make([]any, 100)
		for i := range items {
			items[i] = map[string]any{"metadata": map[string]any{"name": fmt.Sprintf("other-%s-%d", kind, i)}}
		}
		return items
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer read-only-token" ||
			r.URL.Query().Get("limit") != "100" {
			t.Errorf("unbounded or unauthenticated Kubernetes discovery: %s %s", r.Method, r.URL.String())
		}
		pageCalls[r.URL.Path]++
		second := r.URL.Query().Get("continue") != ""
		switch r.URL.Path {
		case "/apis/apps/v1/namespaces/db/deployments":
			if !second {
				writeList(w, dummies("deployment"), "deployment-page-2")
			} else {
				if r.URL.Query().Get("continue") != "deployment-page-2" {
					t.Error("wrong deployment continue token")
				}
				writeList(w, []any{map[string]any{"metadata": map[string]any{"name": "postgres", "uid": "deployment-uid"},
					"spec": map[string]any{"selector": map[string]any{"matchLabels": map[string]any{"app": "postgres"}}}}}, "")
			}
		case "/apis/apps/v1/namespaces/db/statefulsets":
			writeList(w, nil, "")
		case "/apis/apps/v1/namespaces/db/replicasets":
			if !second {
				writeList(w, dummies("replicaset"), "replicaset-page-2")
			} else {
				writeList(w, []any{map[string]any{"metadata": map[string]any{"uid": "rs-uid",
					"ownerReferences": []any{map[string]any{"kind": "Deployment", "uid": "deployment-uid"}}}}}, "")
			}
		case "/api/v1/namespaces/db/pods":
			if !second {
				writeList(w, dummies("pod"), "pod-page-2")
			} else {
				writeList(w, []any{map[string]any{"metadata": map[string]any{"name": "pg-new",
					"labels":          map[string]any{"app": "postgres"},
					"ownerReferences": []any{map[string]any{"kind": "ReplicaSet", "uid": "rs-uid"}}},
					"status": map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}}}, "")
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	writeKubeConfig(t, server.URL, ca, `token: read-only-token`)
	client, err := kubeCredentials(KubernetesTarget{Cluster: "test-context"})
	if err != nil {
		t.Fatal(err)
	}
	pod, err := client.resolveWorkloadPod(context.Background(), "db", "postgres")
	if err != nil || pod != "pg-new" {
		t.Fatalf("Pod on a later list page was missed: %s, %v", pod, err)
	}
	for _, path := range []string{"/apis/apps/v1/namespaces/db/deployments", "/apis/apps/v1/namespaces/db/replicasets", "/api/v1/namespaces/db/pods"} {
		if pageCalls[path] != 2 {
			t.Errorf("expected two pages of %s, got %d", path, pageCalls[path])
		}
	}
}

func TestKubernetesListRejectsRepeatedContinueToken(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"metadata":{"continue":"stuck"},"items":[]}`)
	}))
	defer server.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	writeKubeConfig(t, server.URL, ca, `token: read-only-token`)
	client, err := kubeCredentials(KubernetesTarget{Cluster: "test-context"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.listBounded(context.Background(), "/api/v1/namespaces/db/pods")
	if err == nil || !strings.Contains(err.Error(), "repeated a continue token") {
		t.Fatalf("repeated token was not rejected: %v", err)
	}
}

func TestKubernetesRealReadOnlyCluster(t *testing.T) {
	path := os.Getenv("DBSPHERE_TEST_KUBECONFIG")
	contextName := os.Getenv("DBSPHERE_TEST_K8S_CONTEXT")
	namespace := os.Getenv("DBSPHERE_TEST_K8S_NAMESPACE")
	if path == "" || contextName == "" || namespace == "" {
		t.Skip("read-only Kubernetes test credentials are not configured")
	}
	t.Setenv("DBSPHERE_KUBECONFIG", path)
	client, err := kubeCredentials(KubernetesTarget{Cluster: contextName})
	if err != nil {
		t.Fatal(err)
	}
	pods, err := client.get(context.Background(), "/api/v1/namespaces/"+namespace+"/pods", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range kubeList(pods["items"]) {
		name := kubeString(kubeObject(kubeObject(raw)["metadata"])["name"])
		if name == "" {
			continue
		}
		value := Kubernetes(context.Background(), KubernetesTarget{Cluster: contextName, Namespace: namespace, Pod: name})
		if value["status"] != "ok" || kubeObject(value["pod"])["name"] != name {
			t.Fatalf("read-only Kubernetes sample failed: %+v", value)
		}
		return
	}
	t.Skip("no mapped Pod is running in the test namespace")
}
