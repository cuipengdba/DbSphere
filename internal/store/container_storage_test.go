package store

import (
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func storageFixture(at time.Time, id string, rawRead, active, inactive, refault float64) map[string]any {
	return map[string]any{"collector": map[string]any{"pgStatus": "ok"}, "version_num": 180007, "monitor_role": true,
		"diagnostics": map[string]any{"activity": map[string]any{"status": "ok", "data": []any{map[string]any{"state": "active", "waitType": "IO", "sessions": 1.0}}}},
		"containerStorage": map[string]any{"status": "ok", "runtime": "docker", "node": "qa", "primary": "postgresql",
			"containers": map[string]any{"postgresql": map[string]any{"status": "ok", "source": "cgroup_v2", "id": id, "generation": id,
				"observedAt": at.Format(time.RFC3339Nano), "memory": map[string]any{"active_file": active, "inactive_file": inactive, "workingset_refault_file": refault},
				"pressure": map[string]any{"io_some_avg10": 1.5}, "devices": map[string]any{"8:0": map[string]any{"rbytes": rawRead}}}}}}
}

func TestContainerStorageResetAndRecoveryEvidence(t *testing.T) {
	start := time.Now().UTC()
	before := storageFixture(start, "old", 100, 2048, 512, 2)
	after := storageFixture(start.Add(10*time.Second), "old", 1100, 1024, 500, 7)
	recordContainerStorage(after, before, 10*time.Second)
	storage := object(after["containerStorage"])
	row := object(object(storage["containers"])["postgresql"])
	if object(row["rates"])["readBps"] != 100.0 || object(row["rates"])["active_fileDelta"] != -1024.0 {
		t.Fatalf("invalid interval: %+v", row)
	}
	codes, _ := object(storage["finding"])["codes"].([]string)
	if len(codes) != 1 || codes[0] != "cache_reread" {
		t.Fatalf("unsupported candidate: %+v", storage["finding"])
	}
	reset := storageFixture(start.Add(20*time.Second), "old", 1, 200, 100, 0)
	recordContainerStorage(reset, after, 10*time.Second)
	row = object(object(object(reset["containerStorage"])["containers"])["postgresql"])
	if row["counterReset"] != true || object(row["rates"])["readBps"] != nil {
		t.Fatalf("reset manufactured a rate: %+v", row)
	}
	move := storageFixture(start.Add(30*time.Second), "new", 2000, 2100, 100, 2)
	recordContainerStorage(move, reset, 10*time.Second)
	row = object(object(object(move["containerStorage"])["containers"])["postgresql"])
	if row["identityChanged"] != true || row["rates"] != nil {
		t.Fatalf("new container borrowed old counters: %+v", row)
	}
}

func TestContainerStorageHistoryPreservesPeakAndCoverageOnPG18(t *testing.T) {
	database := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if database == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	ctx := t.Context()
	store, err := Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	raw, err := store.CreateAsset(ctx, assets.Input{Name: "Container storage integration", Host: "127.0.0.1", Port: 5432, Database: "postgres", Username: "monitor", Password: "test-secret", SSLMode: "disable", Runtime: "linux"}, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var asset struct{ ID string }
	if err := json.Unmarshal(raw, &asset); err != nil {
		t.Fatal(err)
	}
	defer store.DeleteAsset(ctx, asset.ID)
	start := time.Now().UTC().Add(-time.Minute)
	for index := 0; index < 4; index++ {
		at := start.Add(time.Duration(index) * 10 * time.Second)
		metrics := storageFixture(at, "one", float64(index*10000), float64(1024-index*20), 512, float64(index))
		metrics["linux"] = map[string]any{"status": "ok", "cpu": map[string]any{"usedPercent": 20 + index}}
		if index == 3 {
			delete(metrics, "containerStorage")
		} else {
			claim := "current-claim"
			if index == 2 {
				claim = "replacement-claim"
			}
			object(metrics["containerStorage"])["pvcs"] = []any{map[string]any{"uid": claim, "name": "data",
				"usage": map[string]any{"status": "ok", "usedBytes": 1234 + index, "capacityBytes": 2000}}}
		}
		if _, err := store.recordCheckAt(ctx, asset.ID, metrics, "", "", at); err != nil {
			t.Fatal(err)
		}
	}
	value, err := store.ReadAssetSection(ctx, asset.ID, "container-storage", url.Values{"from": {start.Add(-time.Second).Format(time.RFC3339Nano)},
		"to": {time.Now().UTC().Format(time.RFC3339Nano)}, "pvcUid": {"current-claim"}})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		Buckets []struct {
			Available, Hosts, Claims int
			Series                   map[string]struct{ Max float64 }
		}
	}
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Buckets) != 4 || response.Buckets[0].Available != 1 || response.Buckets[0].Series["active"].Max != 1024 ||
		response.Buckets[1].Series["readBps"].Max != 1000 || response.Buckets[1].Series["pvcUsed"].Max != 1235 ||
		response.Buckets[2].Claims != 0 || response.Buckets[3].Available != 0 || response.Buckets[3].Hosts != 1 ||
		response.Buckets[3].Series["hostCpu"].Max != 23 {
		t.Fatalf("missing historical coverage or peaks: %s", body)
	}
}
