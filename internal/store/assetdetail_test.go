package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestMetricSeriesPreservesGapsAndCounterResets(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if storeURL == "" {
		t.Skip("PostgreSQL 18 monitoring store is required")
	}
	ctx := context.Background()
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Metric series reset test",
		Host: "127.0.0.1", Port: 1, Database: "postgres", Username: "monitor",
		Password: "test-secret", SSLMode: "disable", Runtime: "linux"},
		[]byte("01234567890123456789012345678901"))
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
	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Hour + 10*time.Minute)
	month := time.Date(base.Year(), base.Month(), 1, 0, 0, 0, 0, time.UTC)
	partition := fmt.Sprintf("samples_%04d%02d", month.Year(), month.Month())
	if _, err := pg.pool.Exec(ctx, fmt.Sprintf(
		"CREATE TABLE IF NOT EXISTS %s PARTITION OF samples FOR VALUES FROM ('%s') TO ('%s')",
		partition, month.Format(time.RFC3339), month.AddDate(0, 1, 0).Format(time.RFC3339))); err != nil {
		t.Fatal(err)
	}
	metric := func(connections, bytes, commits, rollbacks, wal, blocked, long int, lag any) string {
		body, _ := json.Marshal(map[string]any{"connections": connections, "max_connections": 10,
			"database_bytes": bytes, "replication_lag_bytes": lag,
			"xact_commit": commits, "xact_rollback": rollbacks,
			"diagnostics": map[string]any{
				"locks":        map[string]any{"status": "ok", "data": map[string]any{"blockedSessions": blocked}},
				"transactions": map[string]any{"status": "ok", "data": map[string]any{"longTransactions": long}},
				"wal":          map[string]any{"status": "ok", "data": map[string]any{"bytes": fmt.Sprint(wal)}},
			}})
		return string(body)
	}
	samples := []struct {
		minute  int
		status  string
		metrics string
	}{
		{0, "ok", metric(2, 1<<20, 100, 10, 1000, 1, 0, nil)},
		{1, "ok", metric(3, 2<<20, 200, 20, 2000, 2, 1, 2048)},
		{2, "ok", metric(4, 3<<20, 50, 1, 50, 0, 0, 0)}, // counter reset
		{3, "ok", metric(5, 4<<20, 110, 1, 650, 0, 0, 1024)},
		{4, "error", `{}`}, // the next success must not bridge this gap
		{5, "ok", metric(6, 5<<20, 200, 1, 1250, 0, 0, nil)},
	}
	for _, item := range samples {
		if _, err := pg.pool.Exec(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
			VALUES(gen_random_uuid(),$1,$2,$3,$4)`, asset.ID, base.Add(time.Duration(item.minute)*time.Minute),
			item.status, item.metrics); err != nil {
			t.Fatal(err)
		}
	}
	params := url.Values{"from": {base.Add(-time.Minute).Format(time.RFC3339Nano)},
		"to": {base.Add(6 * time.Minute).Format(time.RFC3339Nano)}, "bucketHours": {"1"}}
	value, err := pg.ReadAssetSection(ctx, asset.ID, "series", params)
	if err != nil {
		t.Fatal(err)
	}
	rows := value.(map[string]any)["series"].([]json.RawMessage)
	if len(rows) != 1 {
		t.Fatalf("expected one observed bucket, got %d", len(rows))
	}
	var row map[string]any
	if err := json.Unmarshal(rows[0], &row); err != nil {
		t.Fatal(err)
	}
	check := func(name string, want float64) {
		t.Helper()
		got, ok := row[name].(float64)
		if !ok || math.Abs(got-want) > 0.011 {
			t.Fatalf("%s = %v, want %v (series: %v)", name, row[name], want, row)
		}
	}
	check("samples", 6)
	check("errors", 1)
	check("averageConnections", 4)
	check("averageConnectionUsagePercent", 40)
	check("averageBlockedSessions", 0.6)
	check("averageLongTransactions", 0.2)
	check("averageDatabaseBytes", 3<<20)
	check("averageReplicationLagBytes", 1024)
	check("transactionsPerSecond", 1.42)
	check("walBytesPerSecond", 13.3)
}
