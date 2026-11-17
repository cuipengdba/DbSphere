package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestRealPostgreSQL18Connection(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	store, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	var version, partitions int
	if err := store.pool.QueryRow(context.Background(),
		"SELECT max(version) FROM dbsphere_schema_migrations").Scan(&version); err != nil || version < 13 {
		t.Fatalf("Go schema migration absent: %d %v", version, err)
	}
	if err := store.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_partition_tree('samples'::regclass)").Scan(&partitions); err != nil || partitions < 3 {
		t.Fatalf("monthly sample partitions absent: %d %v", partitions, err)
	}
	hash, err := store.AdminPasswordHash(context.Background(), "first-bootstrap-password")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.AdminPasswordHash(context.Background(), "different-bootstrap-password")
	if err != nil || kept != hash {
		t.Fatal("existing administrator hash was replaced")
	}
	for _, path := range []string{"/api/assets", "/api/events", "/api/alerts", "/api/alert-rules",
		"/api/silences", "/api/notifications", "/api/storage", "/api/inspection-templates",
		"/api/inspection-schedules", "/api/inspection-runs"} {
		value, found, err := store.ReadCollection(context.Background(), path)
		if err != nil || !found {
			t.Fatalf("Go read API %s failed: %v", path, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), "password_enc") || strings.Contains(string(encoded), "pooler_url_enc") {
			t.Fatalf("Go read API %s exposed secrets or invalid JSON: %v", path, err)
		}
	}
}
