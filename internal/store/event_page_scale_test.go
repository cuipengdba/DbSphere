package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

// Run explicitly against a disposable PostgreSQL 18 database:
// DBSPHERE_RUN_EVENT_SCALE=1 DBSPHERE_TEST_PG18_URL=... go test ./internal/store -run TestEventPageScale -v
func TestEventPageScale(t *testing.T) {
	if os.Getenv("DBSPHERE_RUN_EVENT_SCALE") != "1" {
		t.Skip("set DBSPHERE_RUN_EVENT_SCALE=1 for the 100000-event test")
	}
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Fatal("DBSPHERE_TEST_PG18_URL is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	raw, err := pg.CreateAsset(ctx, assets.Input{Name: fmt.Sprintf("Event scale %d", time.Now().UnixNano()),
		Host: "127.0.0.1", Port: 5432, Database: "postgres", Username: "monitor",
		Password: "scale-test-only", SSLMode: "disable", Runtime: "linux"},
		[]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &asset); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if _, err := pg.pool.Exec(cleanupCtx, "DELETE FROM assets WHERE id=$1", asset.ID); err != nil {
			t.Errorf("remove scale fixture: %v", err)
		}
	}()
	now := time.Now().UTC()
	base := time.Date(now.Year(), now.Month(), 15, 12, 0, 0, 0, time.UTC)
	const size = 100000
	insertStart := time.Now()
	_, err = pg.pool.Exec(ctx, `WITH fixture AS MATERIALIZED (
		SELECT gen_random_uuid() AS id, $1::uuid AS asset_id,
		$2::timestamptz - n * interval '1 microsecond' AS started_at,
		CASE WHEN n % 1000 = 0 THEN 'Event scale needle' ELSE 'Event scale ordinary' END AS title
		FROM generate_series(1,$3::int) n
	), keys AS (
		INSERT INTO event_keys(id,asset_id,started_at)
		SELECT id,asset_id,started_at FROM fixture RETURNING id
	)
	INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
	SELECT f.id,f.asset_id,'availability',f.title,'open',f.started_at,'availability'
	FROM fixture f JOIN keys k ON k.id=f.id`, asset.ID, base, size)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("inserted=%d insert_duration=%s", size, time.Since(insertStart))
	if _, err := pg.pool.Exec(ctx, "ANALYZE events"); err != nil {
		t.Fatal(err)
	}
	measure := func(name string, call func() error) {
		t.Helper()
		if err := call(); err != nil {
			t.Fatal(err)
		}
		var total, longest time.Duration
		for range 5 {
			start := time.Now()
			if err := call(); err != nil {
				t.Fatal(err)
			}
			elapsed := time.Since(start)
			total += elapsed
			if elapsed > longest {
				longest = elapsed
			}
		}
		t.Logf("%s average=%s longest=%s", name, total/5, longest)
	}
	measure("first_page", func() error {
		value, err := pg.EventPage(ctx, EventPageQuery{Status: "all", Limit: 50}, []string{asset.ID})
		if err != nil {
			return err
		}
		page := value.(map[string]any)
		if len(page["events"].([]json.RawMessage)) != 50 || !page["hasMore"].(bool) {
			return fmt.Errorf("first page incomplete")
		}
		return nil
	})
	var deepAt time.Time
	var deepID string
	if err := pg.pool.QueryRow(ctx, `SELECT started_at,id::text FROM events WHERE asset_id=$1
		ORDER BY started_at DESC,id DESC OFFSET 99949 LIMIT 1`, asset.ID).Scan(&deepAt, &deepID); err != nil {
		t.Fatal(err)
	}
	deepCursor := base64.RawURLEncoding.EncodeToString([]byte(deepAt.UTC().Format(time.RFC3339Nano) + "|" + deepID))
	measure("deep_page", func() error {
		value, err := pg.EventPage(ctx, EventPageQuery{Status: "all", Cursor: deepCursor, Limit: 50}, []string{asset.ID})
		if err != nil {
			return err
		}
		if len(value.(map[string]any)["events"].([]json.RawMessage)) != 50 {
			return fmt.Errorf("deep page incomplete")
		}
		return nil
	})
	measure("text_search_100_matches", func() error {
		value, err := pg.EventPage(ctx, EventPageQuery{Status: "all", Search: "needle", Limit: 50}, []string{asset.ID})
		if err != nil {
			return err
		}
		if len(value.(map[string]any)["events"].([]json.RawMessage)) != 50 {
			return fmt.Errorf("search page incomplete")
		}
		return nil
	})
	measure("open_count_and_five", func() error {
		value, err := pg.EventOverview(ctx, []string{asset.ID})
		if err != nil {
			return err
		}
		result := value.(map[string]any)
		if result["openCount"].(int64) != size || len(result["latestOpen"].([]json.RawMessage)) != 5 {
			return fmt.Errorf("overview incomplete")
		}
		return nil
	})
}
