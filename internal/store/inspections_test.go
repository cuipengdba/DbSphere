package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestGoInspectionTemplatesSeedAndPreserveThresholds(t *testing.T) {
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
	var count int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*)::int FROM inspection_templates
		WHERE id IN ('pg_daily','linux_daily','k8s_daily','backup_daily')`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("built-in templates not seeded: %d %v", count, err)
	}
	tx, err := pg.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT definition_json FROM inspection_templates WHERE id='pg_daily' FOR UPDATE`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var template inspectionTemplate
	if err := json.Unmarshal(raw, &template); err != nil {
		t.Fatal(err)
	}
	template.CatalogVersion = 1
	template.Version = 12
	template.Checks[0].Threshold = 91
	older, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE inspection_templates SET definition_json=$1 WHERE id='pg_daily'`, older); err != nil {
		t.Fatal(err)
	}
	if err := seedGoInspections(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err := tx.QueryRow(ctx, `SELECT definition_json FROM inspection_templates WHERE id='pg_daily'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &template); err != nil {
		t.Fatal(err)
	}
	if template.Version != 13 || template.CatalogVersion != 3 || template.Checks[0].Threshold != 91 || len(template.Checks) < 15 {
		t.Fatalf("inspection catalog upgrade lost customized threshold: %+v", template)
	}
}

func TestGoInspectionConfigurationInPG18(t *testing.T) {
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
	const templateID = "go_inspection_edit_test"
	template := inspectionTemplate{ID: templateID, Title: "Go edit test", Scope: "linux", Version: 1,
		Checks: []inspectionCheck{{ID: "cpu", Title: "CPU", Metric: "linux.cpu_used_percent",
			Operator: ">=", Threshold: 90, Severity: "warning"}}}
	body, err := json.Marshal(template)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO inspection_templates(id,definition_json,updated_at)
		VALUES($1,$2,now()) ON CONFLICT(id) DO UPDATE SET definition_json=excluded.definition_json`, templateID, body); err != nil {
		t.Fatal(err)
	}
	defer pg.pool.Exec(ctx, `DELETE FROM inspection_templates WHERE id=$1`, templateID)
	if _, err := pg.UpdateInspectionTemplate(ctx, templateID, InspectionTemplatePatch{
		Thresholds: map[string]float64{"missing": 1}}); !errors.Is(err, ErrInvalidInspection) {
		t.Fatalf("unknown inspection check accepted: %v", err)
	}
	if _, err := pg.UpdateInspectionTemplate(ctx, templateID, InspectionTemplatePatch{
		Thresholds: map[string]float64{"cpu": 95}}); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := pg.pool.QueryRow(ctx, `SELECT definition_json FROM inspection_templates WHERE id=$1`, templateID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &template); err != nil {
		t.Fatal(err)
	}
	if template.Version != 2 || template.Checks[0].Threshold != 95 {
		t.Fatalf("threshold edit not persisted: %+v", template)
	}
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Go inspection schedule test", Host: "127.0.0.1", Port: 5432,
		Database: "postgres", Username: "postgres", Password: "test-secret", SSLMode: "disable", Runtime: "linux"},
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
	enabled := true
	if _, err := pg.UpsertInspectionSchedule(ctx, InspectionScheduleInput{AssetID: asset.ID,
		TemplateID: "k8s_daily", IntervalMinutes: 60, Enabled: &enabled}); !errors.Is(err, ErrInvalidInspection) {
		t.Fatalf("cross-runtime schedule accepted: %v", err)
	}
	value, err := pg.UpsertInspectionSchedule(ctx, InspectionScheduleInput{AssetID: asset.ID,
		TemplateID: templateID, IntervalMinutes: 60, Enabled: &enabled})
	if err != nil || value == nil {
		t.Fatalf("valid schedule failed: %v", err)
	}
	var due time.Time
	if err := pg.pool.QueryRow(ctx, `SELECT next_due_at FROM inspection_schedules
		WHERE asset_id=$1 AND template_id=$2`, asset.ID, templateID).Scan(&due); err != nil || due.Before(time.Now()) {
		t.Fatalf("inspection schedule was not persisted: %v %v", due, err)
	}
	claimed, err := pg.ClaimDueInspections(ctx, due.Add(time.Second), 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range claimed {
		if item.AssetID == asset.ID && item.TemplateID == templateID {
			found = true
		}
	}
	if !found {
		t.Fatalf("due inspection not claimed: %+v", claimed)
	}
	again, err := pg.ClaimDueInspections(ctx, due.Add(time.Second), 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range again {
		if item.AssetID == asset.ID && item.TemplateID == templateID {
			t.Fatal("due inspection was claimed twice")
		}
	}
}

func TestGoInspectionRunLinksSamePG18Event(t *testing.T) {
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
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Go inspection outage test", Host: "127.0.0.1", Port: 1,
		Database: "postgres", Username: "postgres", Password: "test-secret", SSLMode: "disable", Runtime: "linux"}, key)
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
	value, err := pg.RunInspection(ctx, asset.ID, "pg_daily", key, "")
	if err != nil {
		t.Fatal(err)
	}
	run := value.(map[string]any)["run"].(map[string]any)
	if run["status"] != "completed" {
		t.Fatalf("inspection failed: %+v", run)
	}
	findings := run["findings"].([]inspectionFinding)
	if len(findings) < 15 || findings[0].ID != "unavailable" || findings[0].Status != "risk" {
		t.Fatalf("outage inspection did not identify availability risk: %+v", findings)
	}
	ids := run["eventIds"].([]string)
	if len(ids) != 1 {
		t.Fatalf("inspection did not link one incident: %+v", ids)
	}
	var storedID, sampleID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text,sample_id::text FROM inspection_runs
		WHERE asset_id=$1`, asset.ID).Scan(&storedID, &sampleID); err != nil ||
		storedID != run["id"] || sampleID != run["sampleId"] {
		t.Fatalf("inspection run/sample not persisted: %s %s %v", storedID, sampleID, err)
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1 AND status='open'`, asset.ID).Scan(&eventID); err != nil || eventID != ids[0] {
		t.Fatalf("inspection linked a different event ID: %s %+v %v", eventID, ids, err)
	}
	repeated, err := pg.RetryInspection(ctx, storedID, key)
	if err != nil {
		t.Fatal(err)
	}
	retry := repeated.(map[string]any)["run"].(map[string]any)
	if retry["retryOf"] != storedID || retry["sampleId"] == sampleID {
		t.Fatalf("inspection retry did not create a new linked sample: %+v", retry)
	}
	report, err := pg.InspectionReport(ctx, storedID, "md")
	if err != nil || !strings.Contains(report, eventID) || !strings.Contains(report, sampleID) ||
		!strings.Contains(report, "数据库无法连接") {
		t.Fatalf("Markdown report omitted source IDs or finding: %v", err)
	}
	htmlReport, err := pg.InspectionReport(ctx, storedID, "html")
	if err != nil || !strings.Contains(htmlReport, eventID) || !strings.Contains(htmlReport, "<table>") {
		t.Fatalf("HTML report omitted source IDs: %v", err)
	}
	failed, err := pg.RunInspection(ctx, asset.ID, "pg_daily", []byte("wrong-master-key-wrong-master-key"), "")
	if err != nil {
		t.Fatal(err)
	}
	failedRun := failed.(map[string]any)["run"].(map[string]any)
	if failedRun["status"] != "failed" || failedRun["sampleId"] != nil ||
		failedRun["error"] == nil {
		t.Fatalf("inspection collection failure was not archived: %+v", failedRun)
	}
	var failedStatus string
	if err := pg.pool.QueryRow(ctx, `SELECT status FROM inspection_runs WHERE id=$1`, failedRun["id"]).
		Scan(&failedStatus); err != nil || failedStatus != "failed" {
		t.Fatalf("failed inspection was not persisted: %s %v", failedStatus, err)
	}
}

func TestGoInspectionReportEscapesUntrustedText(t *testing.T) {
	now := time.Now().UTC()
	finding := inspectionFinding{Title: "bad <script>alert(1)</script>", Status: "unknown",
		Source: "source|with|pipe", Reason: "line\nbreak"}
	htmlBody := htmlInspectionReport("run", "asset <img src=x>", "template", 1, "sample", "",
		now, inspectionSummary{}, []inspectionFinding{finding}, nil)
	if strings.Contains(htmlBody, "<script>") || strings.Contains(htmlBody, "<img src=x>") ||
		!strings.Contains(htmlBody, "&lt;script&gt;") {
		t.Fatal("HTML inspection report did not escape untrusted fields")
	}
	mdBody := markdownInspectionReport("run", "asset", "template", 1, "sample", "",
		now, inspectionSummary{}, []inspectionFinding{finding}, nil)
	if !strings.Contains(mdBody, "source\\|with\\|pipe") || strings.Contains(mdBody, "line\nbreak") {
		t.Fatal("Markdown inspection table did not escape delimiters")
	}
}

func TestGoInspectionComparisonOperators(t *testing.T) {
	template := inspectionTemplate{Checks: []inspectionCheck{
		{ID: "less", Metric: "pg.connection_utilization", Operator: "<=", Threshold: 80},
		{ID: "unknown", Metric: "pg.connection_utilization", Operator: "invalid", Threshold: 80},
	}}
	findings, summary := evaluateGoInspection(template,
		map[string]any{"connections": float64(90), "max_connections": float64(100)}, nil,
		time.Now().UTC(), nil)
	if findings[0].Status != "pass" || findings[1].Status != "unknown" ||
		summary.Pass != 1 || summary.Unknown != 1 {
		t.Fatalf("inspection operators were misclassified: %+v %+v", findings, summary)
	}
}
