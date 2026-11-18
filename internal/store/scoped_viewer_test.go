package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestScopedEventOwnershipReadsPG18(t *testing.T) {
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
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Scoped ownership test", Host: "127.0.0.1",
		Port: 1, Database: "postgres", Username: "monitor", Password: "test-password",
		SSLMode: "disable", Runtime: "linux"}, []byte("01234567890123456789012345678901"))
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
	var eventID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
		VALUES(gen_random_uuid(),$1,now()) RETURNING id::text`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	owner, err := pg.AssetIDForResource(ctx, "event", eventID)
	if err != nil || owner != asset.ID {
		t.Fatalf("event ownership mismatch: %q %v", owner, err)
	}
	if owner, err := pg.AssetIDForResource(ctx, "event", "00000000-0000-0000-0000-000000000000"); err != nil || owner != "" {
		t.Fatalf("unknown event ownership was assumed: %q %v", owner, err)
	}
	if _, err := pg.AssetIDForResource(ctx, "unknown", eventID); err == nil {
		t.Fatal("unknown resource type was accepted")
	}
	hiddenRaw, err := pg.CreateAsset(ctx, assets.Input{Name: "Other scoped ownership test", Host: "127.0.0.2",
		Port: 1, Database: "postgres", Username: "monitor", Password: "test-password",
		SSLMode: "disable", Runtime: "linux"}, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var hidden struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(hiddenRaw, &hidden); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(ctx, hidden.ID)
	if _, err := pg.pool.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,'test','visible old event','resolved',date_trunc('month',now())+interval '1 day','test')`,
		eventID, asset.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `WITH inserted AS (
		INSERT INTO event_keys(id,asset_id,started_at)
		SELECT gen_random_uuid(),$1,date_trunc('month',now())+interval '2 days'
		FROM generate_series(1,205) RETURNING id,asset_id,started_at)
		INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		SELECT id,asset_id,'test','hidden recent event','resolved',started_at,'test' FROM inserted`,
		hidden.ID); err != nil {
		t.Fatal(err)
	}
	value, found, err := pg.ReadScopedCollection(ctx, "/api/events", []string{asset.ID})
	if err != nil || !found {
		t.Fatalf("read scoped events: %v", err)
	}
	rows := value.(map[string]any)["events"].([]json.RawMessage)
	if len(rows) != 1 || !strings.Contains(string(rows[0]), eventID) ||
		strings.Contains(string(rows[0]), hidden.ID) {
		t.Fatalf("asset filter was applied after global LIMIT: %d rows", len(rows))
	}
	value, found, err = pg.ReadScopedCollection(ctx, "/api/assets", []string{asset.ID})
	if err != nil || !found || len(value.(map[string]any)["assets"].([]json.RawMessage)) != 1 {
		t.Fatalf("asset list was not scoped: %v", err)
	}
	if _, _, err := pg.ReadScopedCollection(ctx, "/api/storage", []string{asset.ID}); err == nil {
		t.Fatal("global storage query was exposed to scoped viewer")
	}
}
