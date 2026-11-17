package collector

import (
	"context"
	"net/url"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	diagnostics, ok := metrics["diagnostics"].(map[string]any)
	if !ok {
		t.Fatal("PostgreSQL diagnostic sections missing")
	}
	for _, section := range []string{"activity", "transactions", "locks", "lockChain", "database",
		"wal", "archiver", "checkpoints", "vacuum", "tables", "slots", "freeze",
		"slotDetails", "settings"} {
		entry, ok := diagnostics[section].(map[string]any)
		if !ok || entry["status"] != "ok" {
			t.Fatalf("PostgreSQL %d diagnostic %s unavailable: %v", version/10000, section, entry)
		}
	}
}

func TestRealAdvisoryLockChain(t *testing.T) {
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	if portText == "" {
		t.Skip("PostgreSQL target is required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	address := url.URL{Scheme: "postgres", Host: "127.0.0.1:" + portText,
		Path: "/postgres", User: url.UserPassword("postgres", os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")),
		RawQuery: "sslmode=disable"}
	blocker, err := pgx.Connect(context.Background(), address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(context.Background())
	waiter, err := pgx.Connect(context.Background(), address.String())
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close(context.Background())
	var blockerPID, waiterPID int
	if err := blocker.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if err := waiter.QueryRow(context.Background(), "SELECT pg_backend_pid()").Scan(&waiterPID); err != nil {
		t.Fatal(err)
	}
	lockKey := time.Now().UnixNano()
	if _, err := blocker.Exec(context.Background(), "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
	waitCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		_, _ = waiter.Exec(waitCtx, "SELECT pg_advisory_lock($1)", lockKey)
	}()
	blocked := false
	for range 40 {
		var count int
		if err := blocker.QueryRow(context.Background(), "SELECT cardinality(pg_blocking_pids($1))", waiterPID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			blocked = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("real lock waiter never entered blocked state")
	}
	metrics, err := PostgreSQL(context.Background(), Target{Host: "127.0.0.1", Port: port,
		Database: "postgres", Username: "postgres", SSLMode: "disable"}, os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	diagnostics := metrics["diagnostics"].(map[string]any)
	chain := diagnostics["lockChain"].(map[string]any)
	if chain["status"] != "ok" {
		t.Fatalf("lock chain unavailable: %v", chain)
	}
	linked := false
	for _, item := range chain["data"].([]any) {
		pair := item.(map[string]any)
		if int(pair["waitingPid"].(float64)) == waiterPID && int(pair["blockerPid"].(float64)) == blockerPID {
			linked = true
		}
	}
	if !linked {
		t.Fatalf("Go diagnostics did not capture PID %d blocked by %d: %v", waiterPID, blockerPID, chain)
	}
	cancel()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("lock waiter did not stop")
	}
}
