package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestGoRetentionKeepsEvidenceAndPendingNotifications(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	ctx := context.Background()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	now := time.Now().UTC()
	oldAt, recentAt := now.AddDate(0, 0, -45), now.AddDate(0, 0, -2)
	cutoff := now.AddDate(0, 0, -30)
	if err := pg.EnsureTimePartitions(ctx, oldAt); err != nil {
		t.Fatal(err)
	}
	var assetID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO assets
		(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,created_at)
		VALUES(gen_random_uuid(),'retention test','127.0.0.1',5432,'postgres',
		'tester','unused','disable','linux',now()) RETURNING id::text`).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := pg.pool.Exec(ctx, "DELETE FROM assets WHERE id=$1", assetID); err != nil {
			t.Errorf("clean up retention test asset: %v", err)
		}
	}()
	createSample := func(at time.Time) string {
		t.Helper()
		var id string
		if err := pg.pool.QueryRow(ctx, `INSERT INTO samples(id,asset_id,collected_at,status)
			VALUES(gen_random_uuid(),$1,$2,'ok') RETURNING id::text`, assetID, at).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	unreferenced1, unreferenced2 := createSample(oldAt), createSample(oldAt.Add(time.Minute))
	evidenceSample, openSample := createSample(oldAt), createSample(oldAt)
	inspectionSample, recentSample := createSample(oldAt), createSample(recentAt)
	createEvent := func(status string, at time.Time) string {
		t.Helper()
		var id string
		if err := pg.pool.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
			VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, assetID, at).Scan(&id); err != nil {
			t.Fatal(err)
		}
		var ended any
		if status == "resolved" {
			ended = at.Add(time.Minute)
		}
		if _, err := pg.pool.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,ended_at)
			VALUES($1,$2,'retention_test','retention test',$3,$4,$5)`, id, assetID, status, at, ended); err != nil {
			t.Fatal(err)
		}
		return id
	}
	protectedEvent := createEvent("resolved", oldAt)
	var protectedEvidenceID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'alert_firing',$2,
		jsonb_build_object('nested',jsonb_build_object('baselineSampleId',$3::text)))
		RETURNING id::text`, protectedEvent, oldAt, evidenceSample).Scan(&protectedEvidenceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO notification_outbox
		(id,evidence_id,channel,status,next_attempt_at)
		VALUES(gen_random_uuid(),$1,'webhook','pending',now())`, protectedEvidenceID); err != nil {
		t.Fatal(err)
	}
	openEvent := createEvent("open", oldAt)
	if _, err := pg.pool.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'collection_error',$2,jsonb_build_object('sampleId',$3::text))`,
		openEvent, oldAt, openSample); err != nil {
		t.Fatal(err)
	}
	failedEvent := createEvent("resolved", oldAt)
	var failedEvidenceID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'alert_firing',$2,'{}'::jsonb) RETURNING id::text`,
		failedEvent, oldAt).Scan(&failedEvidenceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO notification_outbox
		(id,evidence_id,channel,status,next_attempt_at)
		VALUES(gen_random_uuid(),$1,'webhook','failed',now())`, failedEvidenceID); err != nil {
		t.Fatal(err)
	}
	groupProtected := createEvent("resolved", oldAt)
	if _, err := pg.pool.Exec(ctx, `INSERT INTO open_event_groups(asset_id,event_group,event_id)
		VALUES($1,'retention-test',$2)`, assetID, groupProtected); err != nil {
		t.Fatal(err)
	}
	inspectionProtected := createEvent("resolved", oldAt)
	deletable1, deletable2 := createEvent("resolved", oldAt), createEvent("resolved", oldAt)
	var deliveredEvidenceID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'alert_recovered',$2,'{}'::jsonb) RETURNING id::text`,
		deletable1, oldAt).Scan(&deliveredEvidenceID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO notification_outbox
		(id,evidence_id,channel,status,next_attempt_at)
		VALUES(gen_random_uuid(),$1,'webhook','delivered',now())`, deliveredEvidenceID); err != nil {
		t.Fatal(err)
	}
	_ = createEvent("resolved", recentAt)
	var templateID string
	if err := pg.pool.QueryRow(ctx, "SELECT id FROM inspection_templates ORDER BY id LIMIT 1").Scan(&templateID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO inspection_runs
		(id,asset_id,template_id,template_version,sample_id,started_at,completed_at,summary_json,findings_json,event_ids_json)
		VALUES(gen_random_uuid(),$1,$2,1,$3,$4,$4,'{}'::jsonb,'[]'::jsonb,jsonb_build_array($5::text))`,
		assetID, templateID, inspectionSample, oldAt, inspectionProtected); err != nil {
		t.Fatal(err)
	}
	pg.ConfigureRetention(30, 60)
	storage, err := pg.storageSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		RetentionDays      int `json:"retentionDays"`
		EventRetentionDays int `json:"eventRetentionDays"`
	}
	if err := json.Unmarshal(storage, &status); err != nil || status.RetentionDays != 30 || status.EventRetentionDays != 60 {
		t.Fatalf("Go storage did not show active retention policy: %+v %v", status, err)
	}
	for _, want := range []int64{1, 1, 0} {
		removed, err := pg.PruneSamples(ctx, cutoff, 1)
		if err != nil || removed != want {
			t.Fatalf("bounded sample retention removed %d, want %d: %v", removed, want, err)
		}
	}
	for _, id := range []string{evidenceSample, openSample, inspectionSample, recentSample} {
		var present bool
		if err := pg.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM samples WHERE id=$1)", id).Scan(&present); err != nil || !present {
			t.Fatalf("protected or recent sample %s was lost: %v", id, err)
		}
	}
	for _, id := range []string{unreferenced1, unreferenced2} {
		var present bool
		if err := pg.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM samples WHERE id=$1)", id).Scan(&present); err != nil || present {
			t.Fatalf("expired unreferenced sample %s remains: %v", id, err)
		}
	}
	for _, want := range []int64{1, 1, 0} {
		removed, err := pg.PruneResolvedEvents(ctx, cutoff, 1)
		if err != nil || removed != want {
			t.Fatalf("bounded event retention removed %d, want %d: %v", removed, want, err)
		}
	}
	for _, id := range []string{protectedEvent, openEvent, failedEvent, groupProtected, inspectionProtected} {
		var present bool
		if err := pg.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM events WHERE id=$1)", id).Scan(&present); err != nil || !present {
			t.Fatalf("protected or open event %s was lost: %v", id, err)
		}
	}
	for _, id := range []string{deletable1, deletable2} {
		var present bool
		if err := pg.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM event_keys WHERE id=$1)", id).Scan(&present); err != nil || present {
			t.Fatalf("expired resolved event %s remains: %v", id, err)
		}
	}
	if _, err := pg.PruneSamples(ctx, cutoff, 0); err == nil {
		t.Fatal("unbounded sample retention was accepted")
	}
	if _, err := pg.PruneResolvedEvents(ctx, cutoff, 1001); err == nil {
		t.Fatal("unbounded event retention was accepted")
	}
}
