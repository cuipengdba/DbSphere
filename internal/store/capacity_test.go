package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestGoCapacityMedianForecastAndInsufficientSignals(t *testing.T) {
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	points := []capacityPoint{{Day: day, DatabaseBytes: 1000, SampleID: "a"},
		{Day: day.AddDate(0, 0, 1), DatabaseBytes: 1100, SampleID: "b"},
		{Day: day.AddDate(0, 0, 2), DatabaseBytes: 1300, SampleID: "c"}}
	forecast := forecastDatabaseCapacity(points)
	if forecast["status"] != "estimate" || forecast["dailyGrowthBytes"] != float64(150) ||
		forecast["projected30DayBytes"] != float64(5800) || forecast["confidence"] != "low" ||
		forecast["latestSampleId"] != "c" {
		t.Fatalf("median capacity estimate differs from persisted history: %+v", forecast)
	}
	if forecastDatabaseCapacity(points[:2])["status"] != "insufficient" {
		t.Fatal("two days of capacity data were presented as a forecast")
	}
	points[2].DatabaseBytes = 900
	if forecastDatabaseCapacity(points)["dailyGrowthBytes"] != float64(50) {
		t.Fatal("capacity decrease was treated as negative growth")
	}
}

func TestGoCapacityForecastReadsPG18DailySamples(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	ctx := context.Background()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Capacity forecast test",
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
	for i := 0; i < 3; i++ {
		at := time.Now().UTC().Add(-time.Duration(2-i)*24*time.Hour - time.Minute)
		month := time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
		next := month.AddDate(0, 1, 0)
		partition := fmt.Sprintf("samples_%04d%02d", month.Year(), month.Month())
		query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF samples FOR VALUES FROM ('%s') TO ('%s')",
			partition, month.Format(time.RFC3339), next.Format(time.RFC3339))
		if _, err := pg.pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.pool.Exec(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
			VALUES(gen_random_uuid(),$1,$2,'ok',$3)`, asset.ID, at,
			fmt.Sprintf(`{"database_bytes":%d}`, 1000+i*100)); err != nil {
			t.Fatal(err)
		}
	}
	value, err := pg.CapacityForecast(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	forecast := value.(map[string]any)
	if forecast["status"] != "estimate" || forecast["dailyGrowthBytes"] != float64(100) ||
		forecast["latestSampleId"] == "" {
		t.Fatalf("Go forecast did not read PG18 daily capacity samples: %+v", forecast)
	}
}
