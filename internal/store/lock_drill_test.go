package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/jackc/pgx/v5"
)

func TestRealLockWaitOpensAndRecoversOneIncident(t *testing.T) {
	if os.Getenv("DBSPHERE_TEST_PG_TARGET_MAJOR") != "18" {
		t.Skip("the sustained real-lock drill runs on the PostgreSQL 18 matrix target")
	}
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	password := os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")
	if storeURL == "" || portText == "" || password == "" {
		t.Skip("PG18 store and target are required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Real Go lock recovery drill",
		Host: "127.0.0.1", Port: port, Database: "postgres", Username: "postgres",
		Password: password, SSLMode: "disable", Runtime: "linux"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(ctx, asset.ID)
	targetURL := url.URL{Scheme: "postgres", Host: "127.0.0.1:" + portText,
		Path: "/postgres", User: url.UserPassword("postgres", password), RawQuery: "sslmode=disable"}
	blocker, err := pgx.Connect(ctx, targetURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(ctx)
	waiter, err := pgx.Connect(ctx, targetURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close(ctx)
	var blockerPID, waitingPID int
	if err := blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if err := waiter.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&waitingPID); err != nil {
		t.Fatal(err)
	}
	lockKey := time.Now().UnixNano()
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	waitDone := make(chan error, 1)
	go func() {
		_, err := waiter.Exec(waitCtx, "SELECT pg_advisory_lock($1)", lockKey)
		waitDone <- err
	}()
	blocked := false
	for range 40 {
		var count int
		if err := blocker.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1))", waitingPID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			blocked = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("real PostgreSQL waiter never blocked")
	}
	first, err := pg.CheckAsset(ctx, asset.ID, key)
	if err != nil || first.Status != "ok" {
		t.Fatalf("first real-lock sample: %+v %v", first, err)
	}
	var count int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*)::int FROM events
		WHERE asset_id=$1 AND event_group='latency'`, asset.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("lock incident opened before the default 60-second duration: %d %v", count, err)
	}
	time.Sleep(61 * time.Second)
	second, err := pg.CheckAsset(ctx, asset.ID, key)
	if err != nil || second.Status != "ok" {
		t.Fatalf("sustained real-lock sample: %+v %v", second, err)
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events
		WHERE asset_id=$1 AND event_group='latency' AND status='open'`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal("sustained real lock did not open one incident", err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	var investigation struct {
		Investigation struct{ Locks *investigationLocks }
	}
	if err := json.Unmarshal(encoded, &investigation); err != nil {
		t.Fatal(err)
	}
	lock := investigation.Investigation.Locks
	if lock == nil || lock.SampleID != second.SampleID || lock.EvidenceID == "" ||
		!slices.ContainsFunc(lock.Pairs, func(pair investigationBlocker) bool {
			return pair.WaitingPID == int(waitingPID) && pair.BlockerPID == int(blockerPID)
		}) {
		t.Fatalf("the investigation shortcut did not cite the real lock sample: %+v", lock)
	}
	for _, expected := range []string{eventID, second.SampleID, `"code":"lock_chain"`,
		`"relation":"blocked_by"`, fmt.Sprint(waitingPID), fmt.Sprint(blockerPID)} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("same-event lock evidence lacks %q: %s", expected, encoded)
		}
	}
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatal("waiting session did not acquire released lock", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("waiting session did not recover after unlock")
	}
	recovery, err := pg.CheckAsset(ctx, asset.ID, key)
	if err != nil || recovery.Status != "ok" {
		t.Fatalf("post-unlock sample: %+v %v", recovery, err)
	}
	var status string
	if err := pg.pool.QueryRow(ctx, `SELECT status FROM events WHERE id=$1`, eventID).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("real lock incident did not resolve under original ID: %s %v", status, err)
	}
	if err := pg.pool.QueryRow(ctx, `SELECT count(*)::int FROM evidence
		WHERE event_id=$1 AND kind='alert_recovered' AND payload_json->>'sampleId'=$2`,
		eventID, recovery.SampleID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovery sample was not linked to the same event: %d %v", count, err)
	}
	if report, err := pg.EventReport(ctx, eventID, "md"); err != nil ||
		!strings.Contains(report, eventID) || !strings.Contains(report, "lock_chain") ||
		!strings.Contains(report, recovery.SampleID) {
		t.Fatal("resolved lock incident report lacks fault and recovery chain", err)
	}
	t.Logf("PG18 real lock incident %s: waiter %d, blocker %d, fault %s, recovery %s",
		eventID, waitingPID, blockerPID, second.SampleID, recovery.SampleID)
}
