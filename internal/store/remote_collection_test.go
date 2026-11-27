package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/remoteobs"
)

func TestPG18RemoteCollectorReplayAndRecovery(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if dsn == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	var asset string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO assets(id,name,host,port,database_name,username,ssl_mode,password_enc,runtime,created_at)
		VALUES (gen_random_uuid(),'Remote collection QA','127.0.0.1',6379,'0','fixture','disable','','redis',now()) RETURNING id::text`).Scan(&asset); err != nil {
		t.Fatal(err)
	}
	defer pg.pool.Exec(ctx, `DELETE FROM assets WHERE id=$1`, asset)
	if err := pg.AssignRemoteCollector(ctx, asset, "site-qa"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.CheckAsset(ctx, asset, nil); !errors.Is(err, ErrCollectionUnavailable) {
		t.Fatalf("local probe unexpectedly ran: %v", err)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE assets SET remote_assigned_at=now()-interval '3 minutes' WHERE id=$1`, asset); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	const interval = 10 * time.Second
	good := json.RawMessage(`{"redis":{"version":"6.2.24","mode":"standalone","role":"primary"},"adapter":{"contract":1,"engine":"redis","version":60224,"role":"primary"}}`)
	first := remoteobs.Observation{Version: remoteobs.Version, ID: "b72d5290-46b6-40ec-8fc3-ba16dbf9133d", AssetID: asset, Engine: "redis", ObservedAt: now.Add(-30 * time.Second), Metrics: good}
	send := func(item remoteobs.Observation, expected string, backlog int) {
		t.Helper()
		receipt, err := pg.StoreRemoteObservation(ctx, "site-qa", item, interval, backlog)
		if err != nil || receipt.ID != item.ID || receipt.Status != expected {
			t.Fatalf("want %s receipt, got %+v: %v", expected, receipt, err)
		}
	}
	if _, err := pg.StoreRemoteObservation(ctx, "other-site", first, interval, 0); !errors.Is(err, ErrRemoteScope) {
		t.Fatalf("wrong site accepted: %v", err)
	}
	send(first, "stored", 2)
	send(first, "duplicate", 1)
	conflict := first
	conflict.ObservedAt = first.ObservedAt.Add(time.Second)
	if _, err := pg.StoreRemoteObservation(ctx, "site-qa", conflict, interval, 0); !errors.Is(err, ErrRemoteConflict) {
		t.Fatalf("duplicate ID accepted with modified sample: %v", err)
	}
	if err := pg.RefreshRemoteCollection(ctx, interval, now); err != nil {
		t.Fatal(err)
	}
	var event string
	if err := pg.pool.QueryRow(ctx, `SELECT e.id::text FROM events e JOIN evidence v ON v.event_id=e.id
		WHERE e.asset_id=$1 AND e.status='open' AND v.kind='alert_firing' AND v.payload_json->>'ruleId'='collector_stale'`, asset).Scan(&event); err != nil {
		t.Fatal(err)
	}
	recovery := first
	recovery.ID, recovery.ObservedAt = "6f556770-9940-45d5-96f2-f41d12e83607", now
	send(recovery, "stored", 0)
	late := first
	late.ID, late.ObservedAt = "570bced9-ca78-4ae3-8633-d358a8143f24", first.ObservedAt.Add(time.Second)
	late.Failure, late.Metrics = "target_unavailable", nil
	send(late, "stored", 0)
	var status string
	var samples int
	if err := pg.pool.QueryRow(ctx, `SELECT last_status,(SELECT count(*) FROM samples WHERE asset_id=$1)
		FROM assets WHERE id=$1`, asset).Scan(&status, &samples); err != nil || status != "ok" || samples != 3 {
		t.Fatalf("latest status %s, samples %d, error %v", status, samples, err)
	}
	var sample string
	if err := pg.pool.QueryRow(ctx, `SELECT v.payload_json->>'sampleId' FROM evidence v JOIN events e ON e.id=v.event_id
		WHERE e.id=$1 AND e.status='resolved' AND v.kind='alert_recovered' AND v.payload_json->>'ruleId'='collector_stale'`, event).Scan(&sample); err != nil || !remoteobs.ValidID(sample) {
		t.Fatalf("same-event recovery raw sample missing: %q %v", sample, err)
	}
}
