package collector

import (
	"context"
	"os"
	"testing"
)

func TestPgBouncerRejectsNonAdminEndpoint(t *testing.T) {
	if _, err := PgBouncer(context.Background(), "postgres://reader:secret@127.0.0.1:6432/postgres", "postgres"); err == nil {
		t.Fatal("non-admin PgBouncer endpoint accepted")
	}
}

func TestPgBouncerRealReadOnlyStats(t *testing.T) {
	connectionURL := os.Getenv("DBSPHERE_TEST_POOLER_URL")
	if connectionURL == "" {
		t.Skip("real PgBouncer admin URL is not configured")
	}
	metrics, err := PgBouncer(context.Background(), connectionURL, "postgres")
	if err != nil {
		t.Fatal(err)
	}
	if metrics["status"] != "ok" || len(metrics["pools"].([]map[string]any)) == 0 {
		t.Fatalf("PgBouncer read-only statistics unavailable: %+v", metrics)
	}
}
