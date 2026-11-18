package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestEventPageFindsOlderHistoryWithoutLeakingScopedAssets(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	ctx := context.Background()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	prefix := fmt.Sprintf("event-page-%d", time.Now().UnixNano())
	createdAssets := []string{}
	defer func() {
		for _, id := range createdAssets {
			if _, err := pg.pool.Exec(ctx, "DELETE FROM assets WHERE id=$1", id); err != nil {
				t.Errorf("remove event page fixture: %v", err)
			}
		}
	}()
	create := func(name string) string {
		raw, err := pg.CreateAsset(ctx, assets.Input{Name: name, Host: "127.0.0.1", Port: 5432,
			Database: "postgres", Username: "monitor", Password: "test-only-password",
			SSLMode: "disable", Runtime: "linux"}, key)
		if err != nil {
			t.Fatal(err)
		}
		var asset struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(raw, &asset); err != nil {
			t.Fatal(err)
		}
		createdAssets = append(createdAssets, asset.ID)
		return asset.ID
	}
	visible := create("Visible " + prefix)
	hidden := create("Hidden " + prefix)
	baseTime := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	insert := func(assetID string, at time.Time, status string) {
		var ended any
		if status == "resolved" {
			ended = at.Add(time.Second)
		}
		_, err := pg.pool.Exec(ctx, `WITH event_key AS (
			INSERT INTO event_keys(id,asset_id,started_at)
			VALUES(gen_random_uuid(),$1,$2) RETURNING id)
			INSERT INTO events(id,asset_id,kind,title,status,started_at,ended_at,event_group)
			SELECT id,$1,'availability',$3,$4,$2,$5,'availability' FROM event_key`,
			assetID, at, prefix, status, ended)
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 205; i++ {
		status := "resolved"
		if i == 0 {
			status = "open"
		}
		insert(visible, baseTime.Add(-time.Duration(i)*time.Second), status)
	}
	insert(hidden, baseTime.Add(time.Second), "resolved")
	var indexed bool
	if err := pg.pool.QueryRow(ctx, "SELECT to_regclass('events_page_order') IS NOT NULL").Scan(&indexed); err != nil || !indexed {
		t.Fatal("event page index missing", err)
	}
	if err := pg.pool.QueryRow(ctx, "SELECT to_regclass('events_asset_page_order') IS NOT NULL AND to_regclass('events_asset_status_page_order') IS NOT NULL").Scan(&indexed); err != nil || !indexed {
		t.Fatal("asset event page indexes missing", err)
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	oldestID := ""
	for {
		value, err := pg.EventPage(ctx, EventPageQuery{Status: "all", Search: prefix,
			Cursor: cursor, Limit: 50}, []string{visible})
		if err != nil {
			t.Fatal(err)
		}
		page := value.(map[string]any)
		rows := page["events"].([]json.RawMessage)
		pages++
		if len(rows) == 0 || len(rows) > 50 {
			t.Fatalf("invalid event page length: %d", len(rows))
		}
		for _, raw := range rows {
			var event struct {
				ID      string `json:"id"`
				AssetID string `json:"assetId"`
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				t.Fatal(err)
			}
			if event.AssetID != visible || seen[event.ID] {
				t.Fatalf("page leaked or repeated event: %+v", event)
			}
			seen[event.ID] = true
			oldestID = event.ID
		}
		if !page["hasMore"].(bool) {
			break
		}
		cursor = page["nextCursor"].(string)
		if cursor == "" {
			t.Fatal("next page cursor missing")
		}
	}
	if len(seen) != 205 || pages != 5 {
		t.Fatalf("wanted 205 scoped events across 5 pages, got %d across %d", len(seen), pages)
	}
	value, _, err := pg.ReadCollection(ctx, "/api/events")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if strings.Contains(string(encoded), oldestID) {
		t.Fatal("oldest event unexpectedly appeared in the legacy 200-row list")
	}
	open, err := pg.EventPage(ctx, EventPageQuery{Status: "open", Search: prefix, Limit: 50}, []string{visible})
	if err != nil || len(open.(map[string]any)["events"].([]json.RawMessage)) != 1 {
		t.Fatal("open status filter failed", err)
	}
	hiddenPage, err := pg.EventPage(ctx, EventPageQuery{Status: "all", Search: "Hidden " + prefix,
		Limit: 50}, []string{visible})
	if err != nil || len(hiddenPage.(map[string]any)["events"].([]json.RawMessage)) != 0 {
		t.Fatal("text search leaked a hidden asset", err)
	}
	all, err := pg.EventPage(ctx, EventPageQuery{Status: "all", Search: prefix, Limit: 100}, nil)
	if err != nil || len(all.(map[string]any)["events"].([]json.RawMessage)) != 100 {
		t.Fatal("unscoped first page failed", err)
	}
	for _, bad := range []EventPageQuery{{Status: "bad", Limit: 50}, {Limit: 0},
		{Limit: 101}, {Cursor: "bad", Limit: 50}} {
		if _, err := pg.EventPage(ctx, bad, []string{visible}); err != ErrInvalidEventPage {
			t.Fatalf("invalid page query accepted: %+v %v", bad, err)
		}
	}
	if _, err := pg.pool.Exec(ctx, "UPDATE events SET status='open',ended_at=NULL WHERE asset_id=$1", visible); err != nil {
		t.Fatal(err)
	}
	overview, err := pg.EventOverview(ctx, []string{visible})
	if err != nil {
		t.Fatal(err)
	}
	result := overview.(map[string]any)
	if result["openCount"].(int64) != 205 || len(result["latestOpen"].([]json.RawMessage)) != 5 {
		t.Fatalf("overview truncated the open event count: %+v", result)
	}
	hiddenOverview, err := pg.EventOverview(ctx, []string{hidden})
	if err != nil || hiddenOverview.(map[string]any)["openCount"].(int64) != 0 {
		t.Fatal("scoped overview leaked another asset", err)
	}
}
