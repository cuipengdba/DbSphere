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
}
