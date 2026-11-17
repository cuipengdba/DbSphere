package store

import (
	"context"
	"os"
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
}
