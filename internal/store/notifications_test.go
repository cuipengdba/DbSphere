package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestGoNotificationOutboxRetryPG18(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	ctx := context.Background()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Go notification test",
		Host: "127.0.0.1", Port: 5432, Database: "postgres", Username: "postgres",
		Password: "test-secret", SSLMode: "disable", Runtime: "linux"},
		[]byte("01234567890123456789012345678901"))
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
	now := time.Now().UTC().Add(-2 * time.Minute)
	metrics := map[string]any{"version_num": 180000, "connections": 1, "max_connections": 100,
		"diagnostics": map[string]any{"locks": map[string]any{"status": "ok",
			"data": map[string]any{"blockedSessions": 1}}}}
	for _, at := range []time.Time{now, now.Add(61 * time.Second)} {
		if _, err := pg.recordCheckAt(ctx, asset.ID, metrics, "", "", at); err != nil {
			t.Fatal(err)
		}
	}
	var webhookID string
	var queued int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*)::int,
		min(n.id::text) FILTER (WHERE n.channel='webhook')
		FROM notification_outbox n JOIN evidence e ON e.id=n.evidence_id
		JOIN events v ON v.id=e.event_id WHERE v.asset_id=$1`, asset.ID).Scan(&queued, &webhookID); err != nil || queued != 2 {
		t.Fatalf("firing alert did not queue two channels: %d %v", queued, err)
	}
	var seen string
	processed, err := pg.DispatchDueNotifications(ctx, 2, func(_ context.Context, item Notification) (string, error) {
		if item.Channel == "webhook" {
			seen = item.ID
			return "", errors.New("local webhook unavailable")
		}
		return "skipped", nil
	})
	if err != nil || processed != 2 || seen != webhookID {
		t.Fatalf("queued deliveries were not processed: %d %s %v", processed, seen, err)
	}
	var status string
	var attempts int
	if err := pg.pool.QueryRow(ctx, `SELECT status,attempts FROM notification_outbox WHERE id=$1`,
		webhookID).Scan(&status, &attempts); err != nil || status != "pending" || attempts != 1 {
		t.Fatalf("failed webhook did not retain retry state: %s %d %v", status, attempts, err)
	}
	for attempt := 2; attempt <= 5; attempt++ {
		if _, err := pg.pool.Exec(ctx, `UPDATE notification_outbox SET next_attempt_at=now()-interval '1 second'
			WHERE id=$1`, webhookID); err != nil {
			t.Fatal(err)
		}
		processed, err := pg.DispatchDueNotifications(ctx, 1, func(_ context.Context, item Notification) (string, error) {
			if item.ID != webhookID {
				t.Fatalf("delivery ID changed on retry: %s", item.ID)
			}
			return "", errors.New("local webhook unavailable")
		})
		if err != nil || processed != 1 {
			t.Fatalf("retry %d did not execute: %d %v", attempt, processed, err)
		}
	}
	if err := pg.pool.QueryRow(ctx, `SELECT status,attempts FROM notification_outbox WHERE id=$1`,
		webhookID).Scan(&status, &attempts); err != nil || status != "failed" || attempts != 5 {
		t.Fatalf("retry cap missing: %s %d %v", status, attempts, err)
	}
	if retried, err := pg.RetryNotification(ctx, webhookID); err != nil || !retried {
		t.Fatalf("manual retry failed: %v %v", retried, err)
	}
	if processed, err := pg.DispatchDueNotifications(ctx, 1, func(_ context.Context, item Notification) (string, error) {
		return "delivered", nil
	}); err != nil || processed != 1 {
		t.Fatalf("manual retry did not deliver: %d %v", processed, err)
	}
	if err := pg.pool.QueryRow(ctx, `SELECT status FROM notification_outbox WHERE id=$1`,
		webhookID).Scan(&status); err != nil || status != "delivered" {
		t.Fatalf("delivery state missing: %s %v", status, err)
	}
}
