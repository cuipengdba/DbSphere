package collector

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCloudNativePGVerifiesOwnershipAndObservedDatabaseRole(t *testing.T) {
	const cluster = `{"metadata":{"name":"cnpg","namespace":"db","uid":"operator-uid"},"spec":{"instances":2},
		"status":{"phase":"Failing over","currentPrimary":"cnpg-1","targetPrimary":"cnpg-2","readyInstances":1,
		"writeService":"cnpg-rw","readService":"cnpg-r","lastSuccessfulBackup":"2026-11-22T00:00:00Z"}}`
	pod := func(name, uid, clusterUID, ip, role string) string {
		return fmt.Sprintf(`{"metadata":{"name":%q,"namespace":"db","uid":%q,"labels":{"cnpg.io/cluster":"cnpg",
			"cnpg.io/instanceName":%q,"cnpg.io/instanceRole":%q},"ownerReferences":[{"kind":"Cluster",
			"apiVersion":"postgresql.cnpg.io/v1","uid":%q,"controller":true}]},"spec":{"containers":[{"ports":[{"containerPort":5432}]}]},
			"status":{"phase":"Running","podIP":%q,"conditions":[{"type":"Ready","status":"True"}]}}`, name, uid, name, role, clusterUID, ip)
	}
	primary := pod("cnpg-1", "old-primary", "operator-uid", "10.10.0.7", "primary")
	replica := pod("cnpg-2", "replica-uid", "operator-uid", "10.10.0.8", "replica")
	foreign := pod("cnpg-foreign", "foreign-uid", "stale-owner", "10.10.0.9", "replica")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer read-only-token" {
			t.Errorf("unexpected Kubernetes API request: %s %s", r.Method, r.URL.Path)
		}
		switch r.URL.Path {
		case "/apis/apps/v1/namespaces/db/deployments", "/apis/apps/v1/namespaces/db/statefulsets", "/api/v1/namespaces/db/services", "/api/v1/namespaces/db/events":
			fmt.Fprint(w, `{"items":[]}`)
		case cnpgAPI + "db/clusters/cnpg":
			fmt.Fprint(w, cluster)
		case cnpgAPI + "db/clusters":
			fmt.Fprintf(w, `{"items":[%s]}`, cluster)
		case "/api/v1/namespaces/db/pods":
			fmt.Fprintf(w, `{"items":[%s,%s,%s]}`, primary, replica, foreign)
		case "/api/v1/namespaces/db/pods/cnpg-1":
			fmt.Fprint(w, primary)
		case "/api/v1/namespaces/db/pods/cnpg-2":
			fmt.Fprint(w, replica)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))
	writeKubeConfig(t, server.URL, ca, `token: read-only-token`)
	target := KubernetesTarget{Cluster: "test-context", Namespace: "db", Workload: "cnpg"}
	ctx := context.Background()
	value := Kubernetes(ctx, target)
	operator := kubeObject(value["cloudNativePG"])
	if value["status"] != "ok" || operator["uid"] != "operator-uid" || len(kubeList(operator["members"])) != 2 ||
		kubeObject(operator["backup"])["status"] != "observed" || operator["targetPrimary"] != "cnpg-2" {
		t.Fatalf("CloudNativePG did not preserve the verified operator report: %+v", value)
	}
	CompareCNPGRole(value, map[string]any{"role": "primary", "postgresEndpoint": map[string]any{"address": "10.10.0.8"}})
	actual := kubeObject(operator["databaseObservation"])
	if actual["status"] != "mismatch" || actual["podUID"] != "replica-uid" || actual["matchesMappedPod"] != false {
		t.Fatalf("PostgreSQL was attributed to the operator's stale primary: %+v", actual)
	}
	CompareCNPGRole(value, map[string]any{"role": "primary", "postgresEndpoint": map[string]any{"address": "10.10.0.9"}})
	if kubeObject(operator["databaseObservation"])["status"] != "unverified" {
		t.Fatal("A Pod with the wrong Cluster owner was considered an observed backend")
	}
	discovery := PreviewCloudNativePG(ctx, target)
	if discovery["status"] != "ok" || len(kubeList(discovery["clusters"])) != 1 {
		t.Fatalf("CloudNativePG discovery preview failed: %+v", discovery)
	}
}
