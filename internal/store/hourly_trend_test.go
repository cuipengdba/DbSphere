package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestHourlySummaryMatchesRawAcrossSlicesAndLateSamples(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if storeURL == "" {
		t.Skip("PostgreSQL 18 monitoring store is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Hourly summary regression", Host: "127.0.0.1",
		Port: 1, Database: "postgres", Username: "monitor", Password: "test-secret", SSLMode: "disable", Runtime: "linux"},
		[]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var asset struct{ ID string }
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	var priorFirst, priorThrough time.Time
	if err := pg.pool.QueryRow(ctx, `SELECT first_hour,through_hour FROM monitoring_hourly_status WHERE singleton`).Scan(&priorFirst, &priorThrough); err != nil {
		t.Fatal(err)
	}
	defer pg.pool.Exec(context.Background(), `UPDATE monitoring_hourly_status SET first_hour=$1,through_hour=$2 WHERE singleton`, priorFirst, priorThrough)
	base := time.Now().UTC().Truncate(time.Hour).Add(-5 * time.Hour)
	month := time.Date(base.Year(), base.Month(), 1, 0, 0, 0, 0, time.UTC)
	if _, err := pg.pool.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS samples_%04d%02d PARTITION OF samples
		FOR VALUES FROM ('%s') TO ('%s')`, month.Year(), month.Month(), month.Format(time.RFC3339), month.AddDate(0, 1, 0).Format(time.RFC3339))); err != nil {
		t.Fatal(err)
	}
	insert := func(minute, counter int) {
		t.Helper()
		metrics := fmt.Sprintf(`{"xact_commit":%d,"xact_rollback":0,"connections":%d,"max_connections":100}`, counter, minute/3)
		if _, err := pg.pool.Exec(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
			VALUES(gen_random_uuid(),$1,$2,'ok',$3::jsonb)`, asset.ID, base.Add(time.Duration(minute)*time.Minute), metrics); err != nil {
			t.Fatal(err)
		}
	}
	for _, minute := range []int{50, 55, 60, 65, 115, 120, 125, 179, 180, 185} {
		insert(minute, minute*10)
	}
	from, to := base.Add(54*time.Minute), base.Add(190*time.Minute)
	ids := []string{asset.ID}
	assertParity := func() {
		t.Helper()
		raw, err := pg.queryJSON(ctx, monitoringBucketsSQL, ids, from, to, 60)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := pg.readMonitoringBuckets(ctx, ids, from, to, 60)
		if err != nil {
			t.Fatal(err)
		}
		if len(actual) != 4 || len(raw) != len(actual) {
			t.Fatalf("expected four buckets across the head, summary and tail, got %d and %d", len(raw), len(actual))
		}
		for i := range raw {
			if !bytes.Equal(raw[i], actual[i]) {
				t.Fatalf("bucket %d: summary differs from raw value\nraw: %s\nsummary: %s", i, raw[i], actual[i])
			}
		}
	}
	rebuild := func(start, end time.Time) {
		t.Helper()
		tx, err := pg.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		if err := rebuildHourlyTrend(ctx, tx, start, end); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE monitoring_hourly_status SET first_hour=$1,through_hour=$2 WHERE singleton`, base, base.Add(3*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	rebuild(base, base.Add(3*time.Hour))
	assertParity()
	insert(119, 100)
	var marked bool
	if err := pg.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM monitoring_hourly_dirty WHERE asset_id=$1 AND bucket=$2)`, asset.ID, base.Add(2*time.Hour)).Scan(&marked); err != nil || !marked {
		t.Fatalf("a late predecessor did not invalidate the next hour: %v", err)
	}
	assertParity()
	rebuild(base.Add(time.Hour), base.Add(3*time.Hour))
	assertParity()
}
