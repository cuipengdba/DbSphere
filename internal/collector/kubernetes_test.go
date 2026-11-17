package collector

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
			fmt.Fprint(w, `{"metadata":{"name":"pg-0","uid":"pod-uid"},"spec":{"nodeName":"worker-1","volumes":[{"persistentVolumeClaim":{"claimName":"pg-data"}}]},"status":{"phase":"Running","containerStatuses":[{"name":"postgres","restartCount":1,"lastState":{"terminated":{"reason":"OOMKilled","finishedAt":"2026-11-17T00:00:00Z"}}}]}}`)
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
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	writeKubeConfig(t, server.URL, ca, `token: read-only-token`)
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
	if len(paths) != 5 {
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
