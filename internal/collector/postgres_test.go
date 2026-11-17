package collector

import (
	"context"
	"os"
	"strconv"
	"testing"
)

func TestReadOnlyPostgreSQLTarget(t *testing.T) {
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	if portText == "" {
		t.Skip("DBSPHERE_TEST_PG_TARGET_PORT is not set")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := PostgreSQL(context.Background(), Target{Host: "127.0.0.1", Port: port,
		Database: "postgres", Username: "postgres", SSLMode: "disable"}, os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	version := metrics["version_num"].(int)
	if version/10000 < 14 || version/10000 > 18 || metrics["role"] != "primary" ||
		metrics["database_bytes"].(int64) <= 0 {
		t.Fatalf("unexpected PostgreSQL read-only metrics: %v", metrics)
	}
	if want := os.Getenv("DBSPHERE_TEST_PG_TARGET_MAJOR"); want != "" && strconv.Itoa(version/10000) != want {
		t.Fatalf("expected PostgreSQL %s, got %d", want, version/10000)
	}
}
