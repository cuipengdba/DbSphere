package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/httpapi"
	"github.com/cuipengdba/dbsphere/internal/store"
	"github.com/jackc/pgx/v5"
)

// This drill starts a real Go HTTP application, a real Go DbSphere OTLP intake,
// PostgreSQL 14–18 as the target and an isolated PostgreSQL 18 monitoring database.
func TestApplicationLockTraceUsesOneDbSphereEventID(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	password := os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")
	if storeURL == "" || portText == "" || password == "" {
		t.Skip("PG18 monitoring store and PostgreSQL target are required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	suffix, err := randomHex(4)
	if err != nil {
		t.Fatal(err)
	}
	dbName := "dbsphere_trace_" + suffix
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatal(err)
	}
	parsedStore, err := url.Parse(storeURL)
	if err != nil {
		t.Fatal(err)
	}
	parsedStore.Path = "/" + dbName
	pg, err := store.Open(ctx, parsedStore.String())
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)")
		t.Fatal(err)
	}
	defer func() {
		pg.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)"); err != nil {
			t.Errorf("remove isolated monitoring database: %v", err)
		}
	}()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Real HTTP lock drill", Host: "127.0.0.1",
		Port: port, Database: "postgres", Username: "postgres", Password: password,
		SSLMode: "disable", Runtime: "linux"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	zero := 0
	if _, err := pg.UpdateAlertRule(ctx, "pg_lock_wait", store.AlertRulePatch{DurationSeconds: &zero}); err != nil {
		t.Fatal(err)
	}
	const token = "real-app-go-otlp-token-at-least-32-characters"
	receiver := httptest.NewServer(httpapi.New(pg, httpapi.AuthConfig{MasterKey: key,
		OTLPToken: token}))
	defer receiver.Close()
	targetURL := (&url.URL{Scheme: "postgres", Host: "127.0.0.1:" + portText,
		Path: "/postgres", User: url.UserPassword("postgres", password),
		RawQuery: "sslmode=disable"}).String()
	blocker, err := pgx.Connect(ctx, targetURL)
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
	app := httptest.NewServer(newHandler(config{pgURL: targetURL,
		endpoint: receiver.URL + "/v1/traces", token: token, lockKey: lockKey},
		&http.Client{Timeout: 5 * time.Second}))
	defer app.Close()
	defer blocker.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
	type response struct {
		code int
		body []byte
		err  error
	}
	done := make(chan response, 1)
	go func() {
		res, err := http.Get(app.URL + "/checkout")
		if err != nil {
			done <- response{err: err}
			return
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		done <- response{code: res.StatusCode, body: body, err: err}
	}()
	var waitingPID int
	for range 80 {
		err := blocker.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE wait_event_type='Lock'
			AND $1 = ANY(pg_blocking_pids(pid)) LIMIT 1`, blockerPID).Scan(&waitingPID)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if waitingPID == 0 {
		t.Fatal("HTTP request did not wait on the real PostgreSQL lock")
	}
	checked, err := pg.CheckAsset(ctx, asset.ID, key)
	if err != nil || checked.Status != "ok" {
		t.Fatalf("DbSphere did not collect the blocked target: %+v %v", checked, err)
	}
	value, known, err := pg.ReadCollection(ctx, "/api/events")
	if err != nil || !known {
		t.Fatal("DbSphere incident list unavailable", err)
	}
	var eventID string
	for _, raw := range value.(map[string]any)["events"].([]json.RawMessage) {
		var item struct {
			ID, AssetID, Status string
		}
		if json.Unmarshal(raw, &item) == nil && item.AssetID == asset.ID && item.Status == "open" {
			eventID = item.ID
		}
	}
	if eventID == "" {
		t.Fatal("the real lock alert did not open one DbSphere incident")
	}
	// The RCA rule intentionally requires a slow database call (at least 500 ms).
	time.Sleep(650 * time.Millisecond)
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.code != http.StatusOK {
			t.Fatalf("HTTP request failed after lock release: %d %s %v", got.code, got.body, got.err)
		}
		var body struct {
			BackendPID int `json:"backendPid"`
		}
		if json.Unmarshal(got.body, &body) != nil || body.BackendPID != waitingPID {
			t.Fatalf("request PID did not match the locked backend: %s", got.body)
		}
	case <-ctx.Done():
		t.Fatal("HTTP request did not recover")
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	for _, marker := range []string{eventID, `"code":"trace_lock_pid_matched"`,
		`"relation":"parent_of"`, `"relation":"same_backend_pid"`,
		`"relation":"blocked_by"`} {
		if !strings.Contains(string(encoded), marker) {
			t.Fatalf("real application lock evidence missing %s in one incident: %s", marker, encoded)
		}
	}
	if recovered, err := pg.CheckAsset(ctx, asset.ID, key); err != nil || recovered.Status != "ok" {
		t.Fatalf("recovery sample failed: %+v %v", recovered, err)
	}
	resolved, err := pg.EventByID(ctx, eventID)
	if err != nil || resolved.(map[string]any)["event"] == nil {
		t.Fatal("original event disappeared during recovery", err)
	}
	resolvedJSON, _ := json.Marshal(resolved)
	if !strings.Contains(string(resolvedJSON), `"status":"resolved"`) {
		t.Fatalf("recovery did not close the same incident ID: %s", resolvedJSON)
	}
}
