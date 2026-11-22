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
	major := version / 10000
	if !PostgresMajorSupported(major) || metrics["role"] != "primary" ||
		metrics["database_bytes"].(int64) <= 0 || metrics["replication_observed"] != true {
		t.Fatalf("unexpected PostgreSQL read-only metrics: %v", metrics)
	}
	if want := os.Getenv("DBSPHERE_TEST_PG_TARGET_MAJOR"); want != "" && strconv.Itoa(version/10000) != want {
		t.Fatalf("expected PostgreSQL %s, got %d", want, version/10000)
	}
	if _, ok := metrics["sqlPerformance"].(map[string]any); !ok {
		t.Fatal("SQL performance capability status missing")
	}
	diagnostics, ok := metrics["diagnostics"].(map[string]any)
	if !ok {
		t.Fatal("PostgreSQL diagnostic sections missing")
	}
	sections := []string{"activity", "activeWaits", "instanceSessions", "transactions", "locks", "lockChain", "database",
		"wal", "archiver", "checkpoints", "vacuum", "tables", "slots", "freeze",
		"slotDetails", "settings", "databaseInventory", "tableInventory", "indexInventory"}
	if version >= 160000 {
		sections = append(sections, "instanceIO")
	}
	for _, section := range sections {
		entry, ok := diagnostics[section].(map[string]any)
		if !ok || entry["status"] != "ok" {
			t.Fatalf("PostgreSQL %d diagnostic %s unavailable: %v", version/10000, section, entry)
		}
	}
	checkpoints := diagnostics["checkpoints"].(map[string]any)["data"].(map[string]any)
	for _, field := range []string{"writeMs", "syncMs", "writtenBuffers"} {
		if _, ok := checkpoints[field].(float64); !ok {
			t.Fatalf("PostgreSQL %d checkpoint metric unavailable: %s", version/10000, field)
		}
	}
	compat := metrics["pgCompatibility"].(map[string]any)
	for _, signal := range compat["signals"].([]postgresSignal) {
		switch signal.Key {
		case "instanceIO":
			if (signal.Status == "ok") != (major >= 16) || (major < 16 && signal.Status != "not_supported") {
				t.Fatalf("unexpected PG%d I/O capability: %+v", major, signal)
			}
		case "checkpoints":
			wantSource := "pg_stat_bgwriter"
			if major >= 17 {
				wantSource = "pg_stat_checkpointer"
			}
			if signal.Source != wantSource || signal.Status != "ok" {
				t.Fatalf("unexpected PG%d checkpoint capability: %+v", major, signal)
			}
		}
	}
}

func TestSQLPerformanceEnabledWithoutRawSQL(t *testing.T) {
	if os.Getenv("DBSPHERE_TEST_SQL_ENABLED") != "1" {
		t.Skip("pg_stat_statements enabled target is required")
	}
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	address := url.URL{Scheme: "postgres", Host: "127.0.0.1:" + portText,
		Path: "/postgres", User: url.UserPassword("postgres", os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")),
		RawQuery: "sslmode=disable"}
	conn, err := pgx.Connect(context.Background(), address.String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(context.Background(), "SELECT 42424242"); err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	target := Target{Host: "127.0.0.1", Port: port, Database: "postgres", Username: "postgres", SSLMode: "disable"}
	metrics, err := PostgreSQL(context.Background(), target, os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD"))
	if err != nil {
		t.Fatal(err)
	}
	performance := metrics["sqlPerformance"].(map[string]any)
	if performance["status"] != "ok" {
		t.Fatalf("enabled pg_stat_statements was unavailable: %v", performance)
	}
	statements := performance["statements"].([]map[string]any)
	if len(statements) == 0 {
		t.Fatal("enabled pg_stat_statements returned no query fingerprints")
	}
	for _, item := range statements {
		if item["fingerprint"] == "" || item["queryId"] == "" {
			t.Fatalf("SQL fingerprint missing: %v", item)
		}
		for _, field := range []string{"query", "queryText", "parameters", "normalizedQuery"} {
			if _, leaked := item[field]; leaked {
				t.Fatalf("raw SQL field %s leaked", field)
			}
		}
	}
	if metrics["version_num"].(int)/10000 == 19 {
		before, _ := performance["resetAt"].(*time.Time)
		if before == nil {
			t.Fatal("PG19 baseline stats reset time is missing")
		}
		if _, err := conn.Exec(context.Background(), "SELECT pg_stat_statements_reset()"); err != nil {
			t.Fatal(err)
		}
		fresh, err := PostgreSQL(context.Background(), target, os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD"))
		if err != nil {
			t.Fatal(err)
		}
		after, _ := fresh["sqlPerformance"].(map[string]any)["resetAt"].(*time.Time)
		if after == nil || !after.After(*before) {
			t.Fatalf("PG19 stats reset time failed to advance from %v to %v", before, after)
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
	if _, err := waiter.Exec(context.Background(), "SET compute_query_id=on"); err != nil {
		t.Fatal(err)
	}
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
	waits := diagnostics["activeWaits"].(map[string]any)
	if waits["status"] != "ok" {
		t.Fatalf("active waits unavailable: %v", waits)
	}
	var queryID string
	if err := blocker.QueryRow(context.Background(), "SELECT query_id::text FROM pg_stat_activity WHERE pid=$1", waiterPID).Scan(&queryID); err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, raw := range waits["data"].(map[string]any)["groups"].([]any) {
		row := raw.(map[string]any)
		if row["waitType"] == "Lock" && row["waitEvent"] == "advisory" && row["queryId"] == queryID {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("actual lock wait and query ID %s were not linked: %v", queryID, waits)
	}
	cancel()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("lock waiter did not stop")
	}
}
