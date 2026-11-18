package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/ai"
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
	aiEvidence, ids, err := pg.EventAIEvidence(ctx, eventID)
	if err != nil || len(ids) == 0 || !strings.Contains(aiEvidence, eventID) ||
		strings.Contains(aiEvidence, "Event report outage") ||
		strings.Contains(aiEvidence, "secret-report-password") {
		t.Fatalf("AI evidence was not redacted or linked: %v", err)
	}
	var usedToday int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM evidence WHERE kind='ai_invocation'
		AND observed_at >= date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'`).Scan(&usedToday); err != nil {
		t.Fatal(err)
	}
	invocationID, err := pg.ReserveAIInvocation(ctx, eventID, "local-model", usedToday+1)
	if err != nil || invocationID == "" {
		t.Fatal("AI budget reservation failed", err)
	}
	if _, err := pg.ReserveAIInvocation(ctx, eventID, "local-model", usedToday+1); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Fatal("daily AI budget was bypassed", err)
	}
	if err := pg.CompleteAIInvocation(ctx, eventID, invocationID, "success", ids[:1]); err != nil {
		t.Fatal("AI audit completion failed", err)
	}
	if err := pg.CompleteAIInvocation(ctx, eventID, invocationID, "failed", nil); !errors.Is(err, ErrEventNotFound) {
		t.Fatal("completed AI audit was overwritten", err)
	}
	var auditCount int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM evidence WHERE event_id=$1 AND kind='ai_invocation'`,
		eventID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("AI invocation was not audited under the same event ID: %d %v", auditCount, err)
	}
	_, allowedAfter, err := pg.EventAIEvidence(ctx, eventID)
	if err != nil || slices.Contains(allowedAfter, invocationID) {
		t.Fatal("a prior AI invocation became citable root cause evidence", err)
	}
}

func TestAutomaticAICriticalEventKeepsOneAuditedAdvisory(t *testing.T) {
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
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Go automatic AI test", Host: "127.0.0.1",
		Port: 1, Database: "postgres", Username: "postgres", Password: "secret-test-password",
		SSLMode: "disable", Runtime: "linux"}, []byte("01234567890123456789012345678901"))
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
	if _, err := pg.CheckAsset(ctx, asset.ID, []byte("01234567890123456789012345678901")); err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, "SELECT id::text FROM events WHERE asset_id=$1 AND status='open'", asset.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	due, err := pg.AutoAIEventIDs(ctx, 20)
	if err != nil || !slices.Contains(due, eventID) {
		t.Fatal("critical event was not queued for auto AI", err)
	}
	_, evidenceIDs, err := pg.EventAIEvidence(ctx, eventID)
	if err != nil || len(evidenceIDs) == 0 {
		t.Fatal("critical event has no citable evidence", err)
	}
	invocationID, err := pg.ReserveAutomaticAIInvocation(ctx, eventID, "test-model", 1000)
	if err != nil || invocationID == "" {
		t.Fatal("auto AI reservation failed", err)
	}
	if _, err := pg.ReserveAutomaticAIInvocation(ctx, eventID, "test-model", 1000); !errors.Is(err, ErrAIAlreadyAnalyzed) {
		t.Fatal("same event was submitted twice for auto AI", err)
	}
	bad := &ai.Result{Summary: "需要核查数据库。", Uncertainty: "还有未知项。",
		EvidenceIDs: []string{"00000000-0000-0000-0000-000000000000"}, Model: "test-model"}
	if err := pg.CompleteAutomaticAIInvocation(ctx, eventID, invocationID, bad); !errors.Is(err, ai.ErrInvalidEvidence) {
		t.Fatal("auto AI accepted fabricated evidence", err)
	}
	good := &ai.Result{Summary: "数据库不可用，需要人工核查。", Uncertainty: "基础设施证据不足。",
		EvidenceIDs: evidenceIDs[:1], SuggestedChecks: []string{"核查 PostgreSQL 进程"}, Model: "test-model"}
	if err := pg.CompleteAutomaticAIInvocation(ctx, eventID, invocationID, good); err != nil {
		t.Fatal("auto AI advisory was not stored", err)
	}
	var advisoryID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM evidence WHERE event_id=$1 AND kind='ai_advisory'`,
		eventID).Scan(&advisoryID); err != nil {
		t.Fatal(err)
	}
	prompt, allowed, err := pg.EventAIEvidence(ctx, eventID)
	if err != nil || slices.Contains(allowed, advisoryID) || slices.Contains(allowed, invocationID) {
		t.Fatal("prior AI output became citable event evidence", err)
	}
	if strings.Contains(prompt, good.Summary) {
		t.Fatal("prior AI suggestion was sent back to the model")
	}
	report, err := pg.EventReport(ctx, eventID, "md")
	if err != nil || !strings.Contains(report, good.Summary) || !strings.Contains(report, "待人工核验") {
		t.Fatal("auto AI advice was absent from the incident report", err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	if !strings.Contains(string(encoded), advisoryID) || !strings.Contains(string(encoded), evidenceIDs[0]) ||
		!strings.Contains(string(encoded), "数据库不可用，需要人工核查") {
		t.Fatalf("auto advice was not retained under one event ID: %s", encoded)
	}
	due, err = pg.AutoAIEventIDs(ctx, 20)
	if err != nil || slices.Contains(due, eventID) {
		t.Fatal("analyzed event was scheduled again", err)
	}
}
