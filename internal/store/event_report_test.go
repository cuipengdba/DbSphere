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

func TestEventReportEscapesNamesAndOmitsRawEvidence(t *testing.T) {
	data := eventReportData{Event: eventRecord{ID: "event-1", AssetID: "asset-1",
		AssetName: "<script>alert(1)</script>", Title: "故障 <img src=x>",
		Status: "open", StartedAt: time.Now().UTC(), Evidence: []eventEvidence{{
			ID: "evidence-1", Kind: "collection_error", ObservedAt: time.Now().UTC(),
			Payload: json.RawMessage(`{"sampleId":"sample-1","sql":"secret raw SQL"}`),
		}}}}
	markdown := renderEventMarkdown(data)
	if strings.Contains(markdown, "<script>") || strings.Contains(markdown, "<img src=x>") ||
		strings.Contains(markdown, "secret raw SQL") || !strings.Contains(markdown, "sample-1") {
		t.Fatal("event report exposed HTML or raw evidence")
	}
}

func TestEventReportLinksSamePG18Incident(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	ctx := context.Background()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Event report outage", Host: "127.0.0.1",
		Port: 1, Database: "postgres", Username: "postgres", Password: "secret-report-password",
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
	defer pg.DeleteAsset(ctx, asset.ID)
	result, err := pg.CheckAsset(ctx, asset.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1 AND status='open'`,
		asset.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	markdown, err := pg.EventReport(ctx, eventID, "md")
	if err != nil || !strings.Contains(markdown, eventID) || !strings.Contains(markdown, result.SampleID) ||
		!strings.Contains(markdown, "未定位") || strings.Contains(markdown, "secret-report-password") {
		t.Fatalf("event report omitted evidence or exposed credential: %v", err)
	}
	htmlReport, err := pg.EventReport(ctx, eventID, "html")
	if err != nil || !strings.Contains(htmlReport, eventID) || !strings.Contains(htmlReport, "<pre>") {
		t.Fatalf("HTML event report was not generated: %v", err)
	}
}
