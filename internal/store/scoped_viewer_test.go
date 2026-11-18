package store

import (
	"context"
	"encoding/json"
	"os"
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
	var asset struct{ ID string `json:"id"` }
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
	if owner, err := pg.AssetIDForResource(ctx, "event", "00000000-0000-0000-0000-000000000000");
		err != nil || owner != "" {
		t.Fatalf("unknown event ownership was assumed: %q %v", owner, err)
	}
	if _, err := pg.AssetIDForResource(ctx, "unknown", eventID); err == nil {
		t.Fatal("unknown resource type was accepted")
	}
}
