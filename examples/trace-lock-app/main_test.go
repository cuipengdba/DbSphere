package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/trace"
	"github.com/jackc/pgx/v5"
)

func TestRealHTTPCheckoutExportsPostgreSQLLockTrace(t *testing.T) {
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	password := os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")
	if portText == "" || password == "" {
		t.Skip("PostgreSQL target is required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	pgURL := (&url.URL{Scheme: "postgres", Host: "127.0.0.1:" + portText,
		Path: "/postgres", User: url.UserPassword("postgres", password),
		RawQuery: "sslmode=disable"}).String()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	blocker, err := pgx.Connect(ctx, pgURL)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(context.Background())
	lockKey := time.Now().UnixNano()
	var blockerPID int
	if err := blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}

	traceBody := make(chan []byte, 1)
	const token = "kind-test-otlp-write-token-32-characters"
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" || r.Header.Get("Authorization") != "Bearer "+token ||
			r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "invalid OTLP request", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		traceBody <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer receiver.Close()
	cfg := config{pgURL: pgURL, endpoint: receiver.URL + "/v1/traces", token: token,
		lockKey: lockKey}
	app := httptest.NewServer(newHandler(cfg, &http.Client{Timeout: 5 * time.Second}))
	defer app.Close()
	defer blocker.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
	type result struct {
		status int
		body   []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		response, err := http.Get(app.URL + "/checkout")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		done <- result{status: response.StatusCode, body: body, err: err}
	}()
	var waitingPID int
	for range 80 {
		query := `SELECT pid FROM pg_stat_activity WHERE wait_event_type='Lock'
			AND $1 = ANY(pg_blocking_pids(pid)) LIMIT 1`
		err := blocker.QueryRow(ctx, query, blockerPID).Scan(&waitingPID)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if waitingPID == 0 {
		t.Fatal("real HTTP request did not wait behind the PostgreSQL lock")
	}
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.status != http.StatusOK {
			t.Fatalf("checkout failed after lock release: %d %s %v", got.status, got.body, got.err)
		}
		var response struct {
			BackendPID int `json:"backendPid"`
		}
		if err := json.Unmarshal(got.body, &response); err != nil || response.BackendPID != waitingPID {
			t.Fatalf("HTTP request was not tied to the blocked PG backend: %s %v", got.body, err)
		}
	case <-ctx.Done():
		t.Fatal("checkout did not complete after the lock was released")
	}
	select {
	case body := <-traceBody:
		if strings.Contains(string(body), "pg_advisory_lock") || strings.Contains(string(body), password) {
			t.Fatal("OTLP export exposed SQL or the PG password")
		}
		decoded, err := trace.Decode(body, []trace.Asset{{ID: "asset", Host: "127.0.0.1",
			Port: port, Database: "postgres"}}, time.Now().UTC())
		if err != nil || len(decoded.Spans) != 2 {
			t.Fatalf("real application OTLP spans were not mapped: %+v %v", decoded, err)
		}
		var parentID string
		for _, span := range decoded.Spans {
			if span.Kind == "service" {
				parentID = span.SpanID
			}
		}
		linked := false
		for _, span := range decoded.Spans {
			if span.Kind == "postgresql_client" && span.BackendPID == waitingPID &&
				span.ParentSpanID == parentID && span.Service == "dbsphere-lock-demo" {
				linked = true
			}
		}
		if !linked {
			t.Fatalf("real HTTP → PG backend link missing: %s", fmt.Sprint(decoded.Spans))
		}
	case <-ctx.Done():
		t.Fatal("application did not export OTLP spans")
	}
}
