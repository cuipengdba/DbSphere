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

func TestPatroniReadsBoundedClusterTopology(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("unexpected Patroni request: %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusMethodNotAllowed)
			return
		}
		user, password, ok := r.BasicAuth()
		if !ok || user != "reader" || password != "private-test-password" {
			t.Error("Patroni Basic auth was not passed to the read-only endpoint")
		}
		switch r.URL.Path {
		case "/cluster":
			fmt.Fprint(w, `{"members":[{"name":"pg-a","role":"leader","state":"running","host":"10.0.0.1","port":5432,"timeline":2},{"name":"pg-b","role":"replica","state":"streaming","host":"10.0.0.2","port":5432,"replay_lag":1024,"tags":{"nosync":true}}],"pause":false}`)
		case "/patroni":
			fmt.Fprintf(w, `{"patroni":{"name":"pg-a"},"dcs_last_seen":%d,"timeline":2,"role":"primary",
				"replication":[{"application_name":"pg-b","sync_state":"async","state":"streaming"}]}`, time.Now().Unix())
		case "/history":
			fmt.Fprint(w, `[[1,9007199254740993,"promotion","2026-03-01T11:00:00Z","pg-a"]]`)
		case "/liveness":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected Patroni path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	endpoint := strings.Replace(server.URL, "http://", "http://reader:private-test-password@", 1)
	result, err := Patroni(context.Background(), endpoint)
	if err != nil || result["status"] != "ok" || result["leaderCount"] != 1 || result["replicaCount"] != 1 {
		t.Fatalf("Patroni topology missing: %+v, %v", result, err)
	}
	if strings.Contains(fmt.Sprint(result), "private-test-password") {
		t.Fatal("Patroni credential leaked into metrics")
	}
	members := result["members"].([]map[string]any)
	if members[1]["replayLagBytes"] != float64(1024) {
		t.Fatalf("replica replay lag missing: %+v", members[1])
	}
	state := result["nodeState"].(map[string]any)
	history := result["history"].(map[string]any)
	if members[1]["nosync"] != true || state["name"] != "pg-a" || state["dcsLastSeen"] == nil ||
		history["entries"].([]map[string]any)[0]["switchLSN"] != "9007199254740993" ||
		result["heartbeat"].(map[string]any)["status"] != "healthy" {
		t.Fatalf("health, eligibility and lossless timeline evidence missing: %+v", result)
	}
}

func TestPatroniRejectsRedirectAndLargeResponse(t *testing.T) {
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://example.org/cluster", http.StatusFound)
	}))
	defer redirect.Close()
	if _, err := Patroni(context.Background(), redirect.URL); err == nil {
		t.Fatal("Patroni collector followed or accepted a redirect")
	}
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"members":[],"extra":"`+strings.Repeat("x", patroniMaxBody)+`"}`)
	}))
	defer large.Close()
	if _, err := Patroni(context.Background(), large.URL); err == nil {
		t.Fatal("Patroni collector accepted an oversized response")
	}
}

func TestPatroniReportsMissingLeaderWithoutInventingCause(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"members":[{"name":"pg-b","role":"replica","state":"streaming"}]}`)
	}))
	defer server.Close()
	result, err := Patroni(context.Background(), server.URL)
	if err != nil || result["leaderCount"] != 0 || result["replicaCount"] != 1 {
		t.Fatalf("missing leader was not preserved as an observed signal: %+v, %v", result, err)
	}
}

func TestPatroniAcceptsUnknownReplicaLag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"members":[{"name":"pg-a","role":"leader","state":"running"},{"name":"pg-b","role":"replica","state":"starting","lag":"unknown","replay_lag":"unknown"}]}`)
	}))
	defer server.Close()
	result, err := Patroni(context.Background(), server.URL)
	if err != nil || result["leaderCount"] != 1 || result["replicaCount"] != 1 {
		t.Fatalf("unknown lag should not invalidate topology: %+v, %v", result, err)
	}
	member := result["members"].([]map[string]any)[1]
	if _, ok := member["replayLagBytes"]; ok {
		t.Fatalf("unknown replay lag was reported as a number: %+v", member)
	}
}
