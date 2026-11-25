package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestNativeRiskUsesSameRoleAndSustainedReadings(t *testing.T) {
	hour := time.Date(2026, 11, 26, 7, 0, 0, 0, time.UTC)
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	signals := riskSignals{Engine: "redis", Location: location, Profile: RiskProfile{TimeZone: "Asia/Shanghai", Start: 9, End: 18}}
	metric := "redis_hit_loss"
	add := func(at time.Time, role string, value float64) {
		signals.Hours = append(signals.Hours, riskHour{At: at, Role: role, Native: map[string]*float64{metric: &value}})
	}
	for at := hour.AddDate(0, 0, -18); at.Before(hour.Add(-5 * time.Hour)); at = at.Add(time.Hour) {
		if at.Hour() == 5 {
			add(at, "replica", 60)
		} else {
			add(at, "master", 3)
		}
	}
	add(hour.Add(-4*time.Hour), "master", 3)
	add(hour.Add(-3*time.Hour), "master", 3)
	add(hour.Add(-2*time.Hour), "master", 3)
	add(hour.Add(-time.Hour), "master", 60)
	spec := nativeRiskSpecs("redis")[1]
	if got := evaluateRiskHour(spec, hour, signals); got.Status != "pending" || got.Baseline != 3 || got.Role != "master" || got.Period != "business" {
		t.Fatalf("isolated spike or mixed role misclassified: %+v", got)
	}
	add(hour, "master", 65)
	if got := evaluateRiskHour(spec, hour.Add(time.Hour), signals); got.Status != "risk" {
		t.Fatalf("sustained hit degradation not detected: %+v", got)
	}
	add(hour.Add(time.Hour), "replica", 3)
	if got := evaluateRiskHour(spec, hour.Add(2*time.Hour), signals); got.Status != "insufficient" {
		t.Fatalf("role switch incorrectly counted as recovery: %+v", got)
	}
}

func TestPG18NativeRiskShadowAndRecovery(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_RISK_PG18_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL 18 forecast database is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	hour := time.Now().UTC().Truncate(time.Hour).Add(time.Hour)
	if err := pg.ensureCollectionPartitions(ctx, hour.AddDate(0, 0, -21)); err != nil {
		t.Fatal(err)
	}
	key := []byte("01234567890123456789012345678901")
	for _, tc := range []struct{ engine, database, metric, normal, elevated string }{
		{"dm8", "DAMENG", "dm8_utilization", `{"role":"primary","dm8":{"files":[{"allocatedBytes":100,"freeBytes":60}]}}`, `{"role":"primary","dm8":{"files":[{"allocatedBytes":100,"freeBytes":1}]}}`},
		{"kingbase", "test", "kingbase_capacity", `{"role":"primary","kingbase":{"databaseBytes":1073741824}}`, `{"role":"primary","kingbase":{"databaseBytes":3221225472}}`},
		{"redis", "0", "redis_eviction_rate", `{"role":"master","redisInterval":{"evicted":0,"seconds":10}}`, `{"role":"master","redisInterval":{"evicted":100,"seconds":10}}`},
		{"etcd", "etcd", "etcd_quota", `{"role":"leader","etcd":{"sizeBytes":30,"quotaBytes":100}}`, `{"role":"leader","etcd":{"sizeBytes":96,"quotaBytes":100}}`},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			raw, err := pg.CreateAsset(ctx, assets.Input{Name: "Native risk " + tc.engine, Host: "127.0.0.1", Port: 1, Database: tc.database,
				Username: "monitor", Password: "fixture-password", SSLMode: "disable", Runtime: tc.engine}, key)
			var asset struct{ ID string }
			if err != nil || json.Unmarshal(raw, &asset) != nil || asset.ID == "" {
				t.Fatalf("create %s asset: %v", tc.engine, err)
			}
			defer pg.DeleteAsset(context.Background(), asset.ID)
			if err := pg.SetRiskProfile(ctx, asset.ID, RiskProfile{TimeZone: "Asia/Shanghai", Start: 0, End: 0}); err != nil {
				t.Fatal(err)
			}
			if _, err := pg.pool.Exec(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
				SELECT gen_random_uuid(),$1,at+interval '1 minute','ok',$3::jsonb FROM generate_series(
				$2::timestamptz-interval '21 days',$2::timestamptz-interval '2 hours',interval '1 hour') at`, asset.ID, hour, tc.normal); err != nil {
				t.Fatal(err)
			}
			insert := func(at time.Time, payload string) string {
				t.Helper()
				var id string
				if err := pg.pool.QueryRow(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
					VALUES(gen_random_uuid(),$1,$2,'ok',$3) RETURNING id::text`, asset.ID, at.Add(time.Minute), payload).Scan(&id); err != nil {
					t.Fatal(err)
				}
				return id
			}
			observe := func(at time.Time, expected string) (id, event string) {
				t.Helper()
				if err := pg.evaluateAssetRisk(ctx, asset.ID, at); err != nil {
					t.Fatal(err)
				}
				var state string
				if err := pg.pool.QueryRow(ctx, `SELECT id::text,coalesce(event_id::text,''),status FROM risk_observations
					WHERE asset_id=$1 AND metric=$2 AND evaluated_at=$3`, asset.ID, tc.metric, at).Scan(&id, &event, &state); err != nil || state != expected {
					t.Fatalf("expected %s, obtained %s: %v", expected, state, err)
				}
				return id, event
			}
			insert(hour.Add(-time.Hour), tc.elevated)
			if _, event := observe(hour, "pending"); event != "" {
				t.Fatal("single window opened an event")
			}
			insert(hour, tc.elevated)
			shadow, event := observe(hour.Add(time.Hour), "risk")
			if event != "" || !errors.Is(pg.SetRiskMode(ctx, asset.ID, true), ErrRiskReviewRequired) {
				t.Fatal("unreviewed baseline could notify")
			}
			if err := pg.ReviewRiskForecast(ctx, asset.ID, shadow, "confirmed"); err != nil {
				t.Fatal(err)
			}
			if err := pg.SetRiskMode(ctx, asset.ID, true); err != nil {
				t.Fatal(err)
			}
			before := insert(hour.Add(time.Hour), tc.elevated)
			_, event = observe(hour.Add(2*time.Hour), "risk")
			if event == "" {
				t.Fatal("risk notification has no event")
			}
			insert(hour.Add(2*time.Hour), tc.normal)
			observe(hour.Add(3*time.Hour), "pending")
			after := insert(hour.Add(3*time.Hour), tc.normal)
			observe(hour.Add(4*time.Hour), "clear")
			value, err := pg.EventByID(ctx, event)
			if err != nil {
				t.Fatal(err)
			}
			replay := object(object(value)["replay"])
			if replay["detection"].(*replayPoint).SampleID != before || replay["recovery"].(*replayPoint).SampleID != after {
				t.Fatalf("lost forecast or recovery source: %+v", replay)
			}
			if object(value)["event"].(*eventRecord).Status != "resolved" {
				t.Fatal("same event did not close")
			}
			t.Logf("%s event=%s first=%s recovery=%s", tc.engine, event, before, after)
		})
	}
}
