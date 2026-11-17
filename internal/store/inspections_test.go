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
}
