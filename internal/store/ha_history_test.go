package store

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestHAHistorySeparatesRouteControlAndReplay(t *testing.T) {
	var measurements map[string]any
	err := json.Unmarshal([]byte(`{
		"role":"standby", "postgresEndpoint":{"address":"127.0.0.1","port":5432},
		"k8s":{"cloudNativePG":{"uid":"cluster","currentPrimary":"pg-1","databaseObservation":{"operatorRole":"primary","status":"mismatch"}}},
		"routing":{"entry":"pg-rw:5432","status":"ok","probe":{"backend":"127.0.0.2:5432","role":"primary"}},
		"diagnostics":{"walPosition":{"data":{"position":"0/123","localReplayGapBytes":0,"replayPaused":true}}}
	}`), &measurements)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	point := haHistoryPoint("k8s", sampleObservation{At: at, Status: "ok", Metrics: measurements})
	if point.Role != "standby" || point.EntryRole != "primary" || point.Control != "pg-1" || !point.Conflict || point.ReplayState != "paused" {
		t.Fatalf("independent HA observations: %+v", point)
	}
	after := point
	after.At = at.Add(time.Second)
	after.Backend = "127.0.0.1:5432"
	if got := haChanges(point, after); !slices.Equal(got, []string{"route"}) {
		t.Fatalf("route transition: %v", got)
	}
	after.At = at.Add(20 * time.Minute)
	if got := haChanges(point, after); !slices.Equal(got, []string{"gap"}) {
		t.Fatalf("observations across gap: %v", got)
	}
}

func TestHAHistoryExactMySQLRouteEvidence(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if dsn == "" {
		t.Skip("PostgreSQL 18 monitoring store is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	store, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	created, err := store.CreateAsset(ctx, assets.Input{Name: "MySQL writer history fixture", Host: "127.0.0.1", Port: 3306,
		Database: "test", Username: "monitor", SSLMode: "disable", Runtime: assets.RuntimeMySQL}, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var asset struct{ ID string }
	if err := json.Unmarshal(created, &asset); err != nil || asset.ID == "" {
		t.Fatalf("decode asset: %v", err)
	}
	defer store.DeleteAsset(ctx, asset.ID)
	start := time.Now().UTC().Add(-time.Minute)
	record := func(offset time.Duration, entry, status string) string {
		t.Helper()
		reading, err := store.recordCheckAt(ctx, asset.ID, map[string]any{
			"mysql": map[string]any{"serverUuid": "replica", "serverReadOnly": true,
				"replication": []map[string]any{{"receivedApplied": true, "receiver": "ON", "applier": "ON"}}},
			"routing": map[string]any{"status": status, "entry": entry, "matchDirect": false},
		}, "", "", start.Add(offset))
		if err != nil {
			t.Fatal(err)
		}
		return reading.SampleID
	}
	bad := record(time.Second, "original:3306", "misrouted")
	unrelated := record(2*time.Second, "another:3306", "ok")
	fixed := record(3*time.Second, "original:3306", "ok")
	params := url.Values{"from": {start.Format(time.RFC3339Nano)}, "to": {start.Add(time.Minute).Format(time.RFC3339Nano)}}
	value, err := store.haHistory(ctx, asset.ID, params)
	if err != nil {
		t.Fatal(err)
	}
	points := value.(map[string]any)["samples"].([]haPoint)
	if len(points) != 3 || points[0].ID != fixed || points[1].ID != unrelated || points[2].ID != bad || string(points[1].Events) != "[]" {
		t.Fatalf("unexpected samples: %+v", points)
	}
	var opened, recovered []struct{ ID, Phase, Status string }
	if err := json.Unmarshal(points[0].Events, &recovered); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(points[2].Events, &opened); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 || len(recovered) != 1 || opened[0].ID != recovered[0].ID || recovered[0].Phase != "recovery" || recovered[0].Status != "resolved" {
		t.Fatalf("recovery evidence: opened=%+v recovered=%+v", opened, recovered)
	}
	params.Set("event", "00000000-0000-4000-8000-000000000003")
	value, err = store.haHistory(ctx, asset.ID, params)
	if err != nil || len(value.(map[string]any)["samples"].([]haPoint)) != 0 {
		t.Fatalf("unknown incident scope: %v %v", value, err)
	}
}
