package store

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestManagedPostgreSQLOnlyCollectsDatabaseSignalsInPG18(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	if storeURL == "" || portText == "" {
		t.Skip("PG18 store and PostgreSQL target are required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := Open(context.Background(), storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(context.Background(), assets.Input{
		Name: "managed fixture", Host: "127.0.0.1", Port: port, Database: "postgres",
		Username: "postgres", Password: os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD"),
		SSLMode: "disable", Runtime: "managed",
	}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID      string `json:"id"`
		Runtime string `json:"runtime"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	if asset.Runtime != "managed" {
		t.Fatalf("managed runtime was not stored: %s", asset.Runtime)
	}
	due, err := pg.DueAssetIDs(context.Background(), time.Minute, 100)
	if err != nil || !slices.Contains(due, asset.ID) {
		t.Fatalf("managed target was not scheduled: %v %v", due, err)
	}
	check, err := pg.CheckAsset(context.Background(), asset.ID, key)
	if err != nil || check.Status != "ok" {
		t.Fatalf("managed target did not produce a healthy PostgreSQL sample: %+v %v", check, err)
	}
	sample, err := pg.RawSample(context.Background(), asset.ID, check.SampleID)
	if err != nil {
		t.Fatal(err)
	}
	var observed struct {
		Sample struct {
			Status  string         `json:"status"`
			Metrics map[string]any `json:"metrics"`
		} `json:"sample"`
	}
	encoded, _ := json.Marshal(sample)
	if err := json.Unmarshal(encoded, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Sample.Status != "ok" || observed.Sample.Metrics["version_num"] == nil ||
		observed.Sample.Metrics["diagnostics"] == nil || observed.Sample.Metrics["linux"] != nil ||
		observed.Sample.Metrics["k8s"] != nil {
		t.Fatalf("managed sample fabricated infrastructure signals or lost PostgreSQL diagnostics: %s", encoded)
	}
}
