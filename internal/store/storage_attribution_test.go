package store

import (
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestStorageAttributionKeepsDeviceAndClaimScopesSeparate(t *testing.T) {
	kube := map[string]any{
		"containerClaims": map[string][]string{"postgres": {"data"}, "backup": {"data"}, "proxy": {}},
		"pvcs":            []any{map[string]any{"name": "data", "uid": "claim-uid", "status": "ok"}},
	}
	links := containerClaimLinks(kube, []storageTarget{{"postgres", "id-a"}, {"proxy", "id-b"}})
	if len(links) != 1 || links[0]["containerId"] != "id-a" || links[0]["pvcUID"] != "claim-uid" ||
		links[0]["sharedInPod"] != true || links[0]["device"] != nil {
		t.Fatalf("bad mount evidence: %+v", links)
	}
	store := map[string]any{"nodeMetricsEndpoint": "node:9100"}
	linux := map[string]any{"endpoint": "other:9100",
		"deviceNumbers": map[string]any{"sda": map[string]any{"number": "8:0"}}}
	container := map[string]any{"devices": map[string]any{"8:0": map[string]any{}}}
	if links := containerDeviceLinks(store, linux, container); len(links) != 0 {
		t.Fatal("a different node exporter matched the device number")
	}
	linux["endpoint"] = "node:9100"
	devices := containerDeviceLinks(store, linux, container)
	if len(devices) != 1 || devices[0]["nodeDevice"] != "sda" || devices[0]["scope"] != "node_device" || devices[0]["pvc"] != nil {
		t.Fatalf("bad node device evidence: %+v", devices)
	}
}

func TestPVCCapacityHistoryOnPG18(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL 18 store required")
	}
	ctx := t.Context()
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	raw, err := pg.CreateAsset(ctx, assets.Input{Name: "PVC history fixture", Host: "127.0.0.1", Port: 5432,
		Database: "postgres", Username: "fixture", Password: "storage-fixture-password", Runtime: "linux", SSLMode: "disable"},
		[]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var asset struct{ ID string }
	if err := json.Unmarshal(raw, &asset); err != nil || asset.ID == "" {
		t.Fatalf("fixture asset: %s %v", asset.ID, err)
	}
	defer pg.DeleteAsset(ctx, asset.ID)
	now := time.Now().UTC().Add(-time.Minute)
	for i := 4; i >= 0; i-- {
		at := now.Add(-time.Duration(i*10) * time.Hour)
		uid := "claim-current"
		if i == 4 {
			uid = "claim-replaced"
		}
		pvc := map[string]any{"name": "data", "status": "ok", "uid": uid, "volume": map[string]any{"uid": "pv-current"},
			"usage": map[string]any{"status": "ok", "seriesKey": "observed-series", "observedAt": at.Format(time.RFC3339Nano),
				"usedBytes": 600 - i*100, "capacityBytes": 1000}}
		metrics := storageFixture(at, "container", 100, 100, 100, 100)
		object(metrics["containerStorage"])["pvcs"] = []any{pvc}
		if _, err := pg.recordCheckAt(ctx, asset.ID, metrics, "", "", at); err != nil {
			t.Fatal(err)
		}
	}
	value, err := pg.containerStorageHistory(ctx, asset.ID, url.Values{"from": {now.Add(-time.Hour).Format(time.RFC3339Nano)},
		"to": {time.Now().UTC().Format(time.RFC3339Nano)}, "pvc": {"data"}, "forecast": {"1"}})
	if err != nil {
		t.Fatal(err)
	}
	capacity := object(value)["pvcCapacity"].([]map[string]any)
	if len(capacity) != 1 || capacity[0]["pvcUID"] != "claim-current" || capacity[0]["status"] != "estimated" ||
		capacity[0]["observations"] != 4 || capacity[0]["hoursToFull"] != 40.0 ||
		capacity[0]["sampleId"] == "" || capacity[0]["baselineSampleId"] == "" {
		t.Fatalf("incorrect capacity estimate or source evidence: %+v", capacity)
	}
}
