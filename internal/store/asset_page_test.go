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

func TestAssetPageFiltersBeforeLimitAndFindsOlderAssets(t *testing.T) {
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
	prefix := fmt.Sprintf("asset-page-%d", time.Now().UnixNano())
	defer func() {
		if _, err := pg.pool.Exec(ctx, "DELETE FROM assets WHERE name LIKE $1", prefix+"%"); err != nil {
			t.Errorf("remove asset page fixture: %v", err)
		}
	}()
	raw, err := pg.CreateAsset(ctx, assets.Input{Name: prefix + "-visible", Host: "127.0.0.1",
		Port: 5432, Database: "postgres", Username: "monitor", Password: "fixture-only-password",
		SSLMode: "disable", Runtime: "linux"}, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var visible struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &visible); err != nil {
		t.Fatal(err)
	}
	_, err = pg.pool.Exec(ctx, `INSERT INTO assets(id,name,host,port,database_name,username,password_enc,
		ssl_mode,runtime,created_at)
		SELECT gen_random_uuid(),$1||'-'||n::text,'127.0.0.1',5432,'postgres','monitor',
		'fixture-only','disable',CASE WHEN n%2=1 THEN 'k8s' ELSE 'linux' END,
		$2::timestamptz+n*interval '1 microsecond'
		FROM generate_series(1,205) n`, prefix, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	var indexed bool
	if err := pg.pool.QueryRow(ctx, "SELECT to_regclass('assets_page_order') IS NOT NULL AND to_regclass('assets_runtime_page_order') IS NOT NULL").Scan(&indexed); err != nil || !indexed {
		t.Fatal("asset pagination indexes missing", err)
	}
	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		value, err := pg.AssetPage(ctx, AssetPageQuery{Search: prefix, Cursor: cursor, Limit: 50}, nil)
		if err != nil {
			t.Fatal(err)
		}
		page := value.(map[string]any)
		rows := page["assets"].([]json.RawMessage)
		if len(rows) == 0 || len(rows) > 50 {
			t.Fatalf("invalid asset page length: %d", len(rows))
		}
		pages++
		for _, row := range rows {
			var asset struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(row, &asset); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(asset.Name, prefix) || seen[asset.ID] {
				t.Fatalf("repeated or unrelated asset: %+v", asset)
			}
			seen[asset.ID] = true
		}
		if !page["hasMore"].(bool) {
			break
		}
		cursor = page["nextCursor"].(string)
		if cursor == "" {
			t.Fatal("next asset cursor missing")
		}
	}
	if len(seen) != 206 || pages != 5 || !seen[visible.ID] {
		t.Fatalf("wanted 206 assets across five pages, got %d across %d", len(seen), pages)
	}
	scoped, err := pg.AssetPage(ctx, AssetPageQuery{Search: prefix, Limit: 50}, []string{visible.ID})
	if err != nil || len(scoped.(map[string]any)["assets"].([]json.RawMessage)) != 1 {
		t.Fatal("scoped asset page failed", err)
	}
	hidden, err := pg.AssetPage(ctx, AssetPageQuery{Search: prefix + "-205", Limit: 50}, []string{visible.ID})
	if err != nil || len(hidden.(map[string]any)["assets"].([]json.RawMessage)) != 0 {
		t.Fatal("scoped asset search leaked a hidden asset", err)
	}
	k8s, err := pg.AssetPage(ctx, AssetPageQuery{Search: prefix, Runtime: "k8s", Limit: 100}, nil)
	if err != nil || len(k8s.(map[string]any)["assets"].([]json.RawMessage)) != 100 || !k8s.(map[string]any)["hasMore"].(bool) {
		t.Fatal("runtime filter failed", err)
	}
	for _, bad := range []AssetPageQuery{{Runtime: "bad", Limit: 50}, {Limit: 0},
		{Limit: 101}, {Cursor: "bad", Limit: 50}} {
		if _, err := pg.AssetPage(ctx, bad, nil); err != ErrInvalidAssetPage {
			t.Fatalf("invalid asset query accepted: %+v %v", bad, err)
		}
	}
}
