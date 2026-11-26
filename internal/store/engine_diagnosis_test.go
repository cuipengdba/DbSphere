package store

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collector"
)

func TestEngineDiagnosisKeepsExactEventAndRedisWindow(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if storeURL == "" {
		t.Skip("PostgreSQL 18 monitoring store is required")
	}
	ctx := context.Background()
	s, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record, err := s.CreateAsset(ctx, assets.Input{Name: "Redis diagnosis fixture", Host: "127.0.0.1", Port: 1,
		Runtime: assets.RuntimeRedis, Database: "0", Username: "default", SSLMode: "disable"}, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var asset struct{ ID string }
	if err := json.Unmarshal(record, &asset); err != nil || asset.ID == "" {
		t.Fatalf("decode fixture asset: %v", err)
	}
	defer s.DeleteAsset(ctx, asset.ID)
	base := time.Now().UTC().Add(-5 * time.Minute)
	collect := func(offset time.Duration, evicted int64) string {
		t.Helper()
		metrics := collector.RedisMetrics{RunID: "test-instance", Mode: "standalone", Version: "6.2.24", Evicted: &evicted}
		point, err := s.recordCheckAt(ctx, asset.ID, map[string]any{"redis": metrics}, "", "", base.Add(offset))
		if err != nil {
			t.Fatal(err)
		}
		return point.SampleID
	}
	collect(0, 10)
	trigger := collect(time.Second, 12)
	coincident := collect(2*time.Second, 14)
	recovered := collect(3*time.Second, 14)
	reset := collect(4*time.Second, 1)
	result, err := s.ReadAssetSection(ctx, asset.ID, "engine-diagnosis", url.Values{
		"from": {base.Add(time.Second).Format(time.RFC3339Nano)},
		"to":   {base.Add(time.Minute).Format(time.RFC3339Nano)},
	})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		SampleID         string
		RedisWindowState string
		Events           []struct{ ID, Phase string }
	}
	byID := map[string]row{}
	for _, raw := range result.(map[string]any)["samples"].([]json.RawMessage) {
		var point row
		if err := json.Unmarshal(raw, &point); err != nil {
			t.Fatal(err)
		}
		byID[point.SampleID] = point
	}
	fired, recovery := byID[trigger].Events, byID[recovered].Events
	if len(byID) != 4 || len(fired) != 1 || fired[0].Phase != "trigger" || len(recovery) != 1 ||
		recovery[0].Phase != "recovery" || recovery[0].ID != fired[0].ID || len(byID[coincident].Events) != 0 ||
		byID[trigger].RedisWindowState != "observed" || byID[reset].RedisWindowState != "counter_reset" {
		t.Fatalf("unexpected diagnosis rows: %+v", byID)
	}
}
