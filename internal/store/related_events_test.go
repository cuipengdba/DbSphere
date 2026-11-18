package store

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestRelatedEventsRequireSameAssetAndOriginalSample(t *testing.T) {
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
	createAsset := func(name string) string {
		t.Helper()
		data, err := pg.CreateAsset(ctx, assets.Input{Name: name, Host: "127.0.0.1", Port: 1,
			Database: "postgres", Username: "monitor", Password: "fixture-password", SSLMode: "disable", Runtime: "linux"},
			[]byte("01234567890123456789012345678901"))
		if err != nil {
			t.Fatal(err)
		}
		var asset struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(data, &asset); err != nil {
			t.Fatal(err)
		}
		return asset.ID
	}
	assetID := createAsset("Related events fixture")
	defer pg.DeleteAsset(ctx, assetID)
	otherAssetID := createAsset("Unrelated events fixture")
	defer pg.DeleteAsset(ctx, otherAssetID)
	var sharedSampleID, otherSampleID string
	for _, target := range []*string{&sharedSampleID, &otherSampleID} {
		if err := pg.pool.QueryRow(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
			VALUES(gen_random_uuid(),$1,now(),'error','{}') RETURNING id::text`, assetID).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	createEvent := func(id, group, sampleID string) string {
		t.Helper()
		started := time.Now().UTC()
		var eventID string
		if err := pg.pool.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
			VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, id, started).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.pool.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
			VALUES($1,$2,'availability','Related sample fixture','open',$3,$4)`, eventID, id, started, group); err != nil {
			t.Fatal(err)
		}
		if _, err := pg.pool.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'alert_firing',now(),jsonb_build_object('sampleId',$2::text))`, eventID, sampleID); err != nil {
			t.Fatal(err)
		}
		return eventID
	}
	mainID := createEvent(assetID, "availability", sharedSampleID)
	sharedID := createEvent(assetID, "telemetry", sharedSampleID)
	_ = createEvent(assetID, "telemetry", otherSampleID)
	_ = createEvent(otherAssetID, "telemetry", sharedSampleID)
	load := func(eventID string) []relatedEventRecord {
		t.Helper()
		detail, err := pg.EventByID(ctx, eventID)
		if err != nil {
			t.Fatal(err)
		}
		return detail.(map[string]any)["relatedEvents"].([]relatedEventRecord)
	}
	if related := load(mainID); len(related) != 1 || related[0].ID != sharedID ||
		related[0].SharedSampleID != sharedSampleID {
		t.Fatalf("exact shared sample/asset link missing or polluted: %+v", related)
	}
	if related := load(sharedID); len(related) != 1 || related[0].ID != mainID {
		t.Fatalf("reverse related event link missing: %+v", related)
	}
	report, err := pg.EventReport(ctx, mainID, "md")
	if err != nil || !strings.Contains(report, sharedID) || !strings.Contains(report, sharedSampleID) {
		t.Fatal("report omitted exact related event link", err)
	}
	aiPrompt, _, err := pg.EventAIEvidence(ctx, mainID)
	if err != nil || strings.Contains(aiPrompt, sharedID) {
		t.Fatal("AI evidence included unrelated incident detail", err)
	}
}
