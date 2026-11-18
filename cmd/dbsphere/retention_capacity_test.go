package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/store"
	"github.com/jackc/pgx/v5"
)

// This dedicated PG18 test verifies that the hourly worker can catch up after
// more than one batch of expired samples. It runs only in the capacity job.
func TestRunDataRetentionDrainsBacklog(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if os.Getenv("DBSPHERE_TEST_PG18_CAPACITY") != "1" || url == "" {
		t.Skip("dedicated PostgreSQL 18 capacity fixture is not configured")
	}
	ctx := context.Background()
	pg, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	month := time.Now().UTC().AddDate(0, -4, 0)
	oldAt := time.Date(month.Year(), month.Month(), 1, 12, 0, 0, 0, time.UTC)
	if err := pg.EnsureTimePartitions(ctx, oldAt); err != nil {
		t.Fatal(err)
	}
	var assetID string
	if err := conn.QueryRow(ctx, `INSERT INTO assets
		(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,created_at)
		VALUES(gen_random_uuid(),'retention capacity','127.0.0.1',5432,'postgres',
		'tester','unused','disable','linux',now()) RETURNING id::text`).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := conn.Exec(ctx, "DELETE FROM assets WHERE id=$1", assetID); err != nil {
			t.Errorf("clean up retention capacity asset: %v", err)
		}
	}()
	const backlog = 120000
	if _, err := conn.Exec(ctx, `INSERT INTO samples(id,asset_id,collected_at,status)
		SELECT gen_random_uuid(),$1,$2::timestamptz + g * interval '1 second','ok'
		FROM generate_series(1,$3) AS g`, assetID, oldAt, backlog); err != nil {
		t.Fatal(err)
	}
	const eventBacklog = 5000
	if _, err := conn.Exec(ctx, `WITH keys AS (
		INSERT INTO event_keys(id,asset_id,started_at)
		SELECT gen_random_uuid(),$1,$2::timestamptz + g * interval '1 second'
		FROM generate_series(1,$3) AS g
		RETURNING id,asset_id,started_at
	) INSERT INTO events(id,asset_id,kind,title,status,started_at,ended_at)
	SELECT id,asset_id,'capacity','retention backlog','resolved',started_at,
		started_at + interval '1 minute' FROM keys`, assetID, oldAt, eventBacklog); err != nil {
		t.Fatal(err)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		runDataRetention(workerCtx, pg, 90, 90, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	defer func() {
		cancel()
		<-done
	}()
	deadline := time.Now().Add(2 * time.Minute)
	var remainingSamples, remainingEvents int
	for time.Now().Before(deadline) {
		queryCtx, queryCancel := context.WithTimeout(ctx, 5*time.Second)
		err := conn.QueryRow(queryCtx, `SELECT
			(SELECT count(*) FROM samples WHERE asset_id=$1),
			(SELECT count(*) FROM events WHERE asset_id=$1)`, assetID).
			Scan(&remainingSamples, &remainingEvents)
		queryCancel()
		if err != nil {
			t.Fatal(err)
		}
		if remainingSamples == 0 && remainingEvents == 0 {
			t.Logf("drained %d samples and %d resolved events in %s",
				backlog, eventBacklog, time.Since(started).Round(time.Millisecond))
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("hourly retention backlog remains: %d samples and %d events",
		remainingSamples, remainingEvents)
}
