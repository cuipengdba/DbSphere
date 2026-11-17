package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
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
