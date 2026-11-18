package store

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestSlowLogAndPlanSanitization(t *testing.T) {
	line := `{"timestamp":"2026-11-18T00:00:00Z","dbname":"app","message":"duration: 123.5 ms  statement: SELECT secret FROM private","query_id":42,"user":"private-user"}`
	entries, scanned, err := parseSlowLogs("jsonlog", line+"\n", "app")
	if err != nil || scanned != 1 || len(entries) != 1 || entries[0].DurationMs != 123.5 || *entries[0].QueryID != "42" {
		t.Fatalf("jsonlog did not retain safe evidence: %d %+v %v", scanned, entries, err)
	}
	if entries[0].SourceKey == "" || strings.Contains(entries[0].SourceKey, "secret") {
		t.Fatal("raw statement appeared in source key")
	}
	if _, _, err := parseSlowLogs("jsonlog", "not-json", "app"); !errors.Is(err, ErrInvalidSlowLog) {
		t.Fatal("malformed JSON log was accepted")
	}
	fields := []string{"2026-11-18 00:00:00.000 UTC", "user", "app", "pid"}
	for len(fields) < 26 {
		fields = append(fields, "")
	}
	fields[13], fields[25] = "duration: 9.5 ms  statement: select password", "9001"
	entry, keep, err := parseSlowLogRecord("csvlog", strings.Join(fields, ","), "app", fields)
	if err != nil || !keep || entry.QueryID == nil || *entry.QueryID != "9001" {
		t.Fatalf("csvlog was not parsed: %+v %v", entry, err)
	}
	fields[13] = "duration: 9.5 ms  statement: select 'a,b'\nfrom private_table"
	var csvBody bytes.Buffer
	writer := csv.NewWriter(&csvBody)
	if err := writer.Write(fields); err != nil {
		t.Fatal(err)
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		t.Fatal(err)
	}
	csvEntries, csvScanned, err := parseSlowLogs("csvlog", csvBody.String(), "app")
	if err != nil || csvScanned != 1 || len(csvEntries) != 1 || csvEntries[0].DurationMs != 9.5 {
		t.Fatalf("multiline csvlog failed: scanned=%d entries=%+v err=%v", csvScanned, csvEntries, err)
	}
	plan := json.RawMessage(`[ {"Plan":{"Node Type":"Seq Scan","Relation Name":"sensitive_table","Filter":"secret = 'x'","Total Cost":10,"Plans":[{"Node Type":"Result","Actual Rows":3}]},"Execution Time":7.2,"Query Text":"SELECT secret"} ]`)
	_, shape, summary, err := sanitizeSQLPlan("42", "2026-11-18T00:00:00Z", plan)
	if err != nil || shape == "" || !strings.Contains(string(summary), `"nodeCount":2`) {
		t.Fatalf("plan summary failed: %s %v", summary, err)
	}
	for _, secret := range []string{"sensitive_table", "secret", "SELECT", "Filter"} {
		if strings.Contains(string(summary), secret) {
			t.Fatalf("plan summary leaked %s", secret)
		}
	}
	for _, invalid := range []string{`{}`, `{"Plan":{"Node Type":"Seq Scan","Plans":["bad"]}}`, `{"Plan":{"Node Type":"Seq Scan;DROP"}}`} {
		if _, _, _, err := sanitizeSQLPlan("42", "2026-11-18T00:00:00Z", json.RawMessage(invalid)); !errors.Is(err, ErrInvalidSQLPlan) {
			t.Fatalf("invalid plan accepted: %s", invalid)
		}
	}
}

func TestSQLImportsPersistOnlySafeEvidenceInPG18(t *testing.T) {
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
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "SQL import fixture", Host: "127.0.0.1", Port: 5432, Database: "sql_import_fixture", Username: "monitor", Password: "fixture-password", SSLMode: "disable", Runtime: "linux"}, []byte("01234567890123456789012345678901"))
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
	line := `{"timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","dbname":"sql_import_fixture","message":"duration: 1500 ms  statement: SELECT confidential_value","query_id":"42"}`
	first, err := pg.ImportSlowLogs(ctx, asset.ID, "jsonlog", line)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pg.ImportSlowLogs(ctx, asset.ID, "jsonlog", line)
	if err != nil {
		t.Fatal(err)
	}
	if first.(map[string]any)["imported"] != 1 || second.(map[string]any)["duplicates"] != 1 {
		t.Fatal("slow log deduplication failed")
	}
	plan, err := pg.ImportSQLPlan(ctx, asset.ID, "42", time.Now().UTC().Format(time.RFC3339Nano), json.RawMessage(`{"Plan":{"Node Type":"Seq Scan","Filter":"confidential_value = 1","Actual Rows":1},"Execution Time":1500}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(plan)
	if strings.Contains(string(encoded), "confidential_value") || !strings.Contains(string(encoded), `"queryId":"42"`) {
		t.Fatal("plan leaked raw predicate or lost identity")
	}
	_, err = pg.ImportSQLPlan(ctx, asset.ID, "42", time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano),
		json.RawMessage(`{"Plan":{"Node Type":"Index Scan","Index Name":"confidential_index"},"Execution Time":3000}`))
	if err != nil {
		t.Fatal(err)
	}
	listed, err := pg.ReadAssetSection(ctx, asset.ID, "sql-plans", nil)
	if err != nil {
		t.Fatal(err)
	}
	listedJSON, _ := json.Marshal(listed)
	if strings.Contains(string(listedJSON), "confidential_index") ||
		!strings.Contains(string(listedJSON), `"shapeChanged":true`) ||
		!strings.Contains(string(listedJSON), `"executionRatio":2`) {
		t.Fatalf("safe plan comparison missing: %s", listedJSON)
	}
	eventStart := time.Now().UTC().Add(-2 * time.Minute)
	baseline, err := pg.ImportSQLPlan(ctx, asset.ID, "42", eventStart.Add(-time.Hour).Format(time.RFC3339Nano),
		json.RawMessage(`{"Plan":{"Node Type":"Seq Scan"},"Execution Time":500}`))
	if err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
		VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, asset.ID, eventStart).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	defer pg.pool.Exec(ctx, `DELETE FROM event_keys WHERE id=$1`, eventID)
	if _, err := pg.pool.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,'sql_performance','SQL import correlation','open',$3,'latency')`, eventID, asset.ID, eventStart); err != nil {
		t.Fatal(err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	detailJSON, _ := json.Marshal(detail)
	baselineJSON, _ := json.Marshal(baseline)
	var baselineRecord struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(baselineJSON, &baselineRecord); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(detailJSON), `"code":"sql_plan_regression"`) ||
		!strings.Contains(string(detailJSON), `"plan:`+baselineRecord.ID+`"`) ||
		!strings.Contains(string(detailJSON), `"relation":"compared_with"`) ||
		strings.Contains(string(detailJSON), "confidential_value") {
		t.Fatalf("event did not link safe SQL RCA evidence: %s", detailJSON)
	}
	if err := pg.RecordRCADecision(ctx, eventID, "sql_plan_regression", "confirmed", "计划变化与慢日志同窗，已由值班人员核实"); err != nil {
		t.Fatalf("SQL hypothesis could not be reviewed: %v", err)
	}
	var queryID string
	var count int
	if err := pg.pool.QueryRow(ctx, `SELECT query_id FROM slow_log_entries WHERE asset_id=$1`, asset.ID).Scan(&queryID); err != nil || queryID != "42" {
		t.Fatalf("query ID lost: %q %v", queryID, err)
	}
	if err := pg.pool.QueryRow(ctx, `SELECT count(*)::int FROM slow_log_entries WHERE asset_id=$1`, asset.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate log rows: %d %v", count, err)
	}
	var stored string
	if err := pg.pool.QueryRow(ctx, `SELECT summary_json::text FROM sql_plan_snapshots WHERE asset_id=$1 ORDER BY observed_at LIMIT 1`, asset.ID).Scan(&stored); err != nil || strings.Contains(stored, "confidential_value") {
		t.Fatalf("unsafe plan stored: %v", err)
	}
	if _, err := pg.pool.Exec(ctx, `DELETE FROM slow_log_entries WHERE asset_id=$1`, asset.ID); err != nil {
		t.Fatal(err)
	}
	withoutLog, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range object(withoutLog.(map[string]any)["analysis"])["candidates"].([]map[string]any) {
		if candidate["code"] == "sql_plan_regression" {
			t.Fatal("plan change alone was misrepresented as a corroborated SQL candidate")
		}
	}
}
