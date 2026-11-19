package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/jackc/pgx/v5"
)

func sqlRegressionFixture(calls, total float64, reset string) map[string]any {
	return map[string]any{"status": "ok", "resetAt": reset, "statements": []any{map[string]any{
		"fingerprint": "1:2:3:1", "queryId": "3", "calls": calls, "totalExecMs": total,
		"rows": float64(0), "sharedBlocksHit": float64(0), "sharedBlocksRead": float64(0),
		"sharedBlocksWritten": float64(0), "tempBlocksRead": float64(0),
		"tempBlocksWritten": float64(0), "walBytes": float64(0)}}}
}

func TestSQLRegressionRequiresTwoComparableIntervalsAndRecovers(t *testing.T) {
	oldest := sqlRegressionFixture(1, 10, "reset-a")
	previous := sqlRegressionFixture(6, 60, "reset-a")
	current := sqlRegressionFixture(11, 560, "reset-a")
	regression := sqlRegressionSignal(current, previous, oldest)
	if regression == nil || regression["maxRatio"] != float64(10) ||
		stringValue(object(regression["worst"])["queryId"]) != "3" {
		t.Fatalf("two comparable intervals did not identify the slow fingerprint: %v", regression)
	}
	if signal := readGoSignal("pg.sql_regression_ratio", map[string]any{"sqlRegression": regression},
		time.Now()); !signal.Available || signal.Value != 10 {
		t.Fatalf("SQL regression alert signal missing: %+v", signal)
	}
	recovered := sqlRegressionSignal(sqlRegressionFixture(16, 610, "reset-a"), current, previous)
	if recovered == nil || recovered["maxRatio"] != float64(0) {
		t.Fatalf("new fast interval did not explicitly clear regression: %v", recovered)
	}
	for name, changed := range map[string]map[string]any{
		"reset":        sqlRegressionFixture(11, 560, "reset-b"),
		"missing":      {"status": "not_configured"},
		"insufficient": sqlRegressionFixture(8, 260, "reset-a"),
	} {
		if result := sqlRegressionSignal(changed, previous, oldest); result != nil {
			t.Errorf("%s telemetry was treated as a comparable healthy interval: %v", name, result)
		}
	}
}

func TestRealPG18SQLRegressionOpensAndRecoversOneIncident(t *testing.T) {
	if os.Getenv("DBSPHERE_TEST_PG_TARGET_MAJOR") != "18" {
		t.Skip("real SQL regression drill runs on the PostgreSQL 18 matrix target")
	}
	storeURL, portText := os.Getenv("DBSPHERE_TEST_PG18_URL"), os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	password := os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")
	if storeURL == "" || portText == "" || password == "" {
		t.Skip("PG18 store and pg_stat_statements target are required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Real Go SQL regression drill",
		Host: "127.0.0.1", Port: port, Database: "postgres", Username: "postgres",
		Password: password, SSLMode: "disable", Runtime: "linux"}, key)
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
	targetURL := url.URL{Scheme: "postgres", Host: "127.0.0.1:" + portText,
		Path: "/postgres", User: url.UserPassword("postgres", password), RawQuery: "sslmode=disable"}
	target, err := pgx.Connect(ctx, targetURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close(ctx)
	const query = "SELECT pg_sleep($1::double precision), 8675309"
	if _, err := target.Exec(ctx, query, 0.12); err != nil {
		t.Fatal(err)
	}
	collect := func(label string) CheckResult {
		t.Helper()
		result, err := pg.CheckAsset(ctx, asset.ID, key)
		if err != nil || result.Status != "ok" {
			t.Fatalf("%s real PG18 collection: %+v %v", label, result, err)
		}
		return result
	}
	initial := collect("initial")
	run := func(seconds float64) {
		t.Helper()
		for range 5 {
			if _, err := target.Exec(ctx, query, seconds); err != nil {
				t.Fatal(err)
			}
		}
	}
	run(0.12)
	baseline := collect("baseline")
	run(1.2)
	fault := collect("fault")
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1
		AND event_group='latency' AND status='open'`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal("real SQL latency regression did not open an incident", err)
	}
	var regression map[string]any
	var raw []byte
	if err := pg.pool.QueryRow(ctx, `SELECT metrics_json FROM samples WHERE id=$1`, fault.SampleID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var metrics map[string]any
	if err := json.Unmarshal(raw, &metrics); err != nil {
		t.Fatal(err)
	}
	regression = object(metrics["sqlRegression"])
	if regression["previousSampleId"] != baseline.SampleID ||
		regression["baselineSampleId"] != initial.SampleID || number(regression["maxRatio"]) < 2 ||
		stringValue(object(regression["worst"])["queryId"]) == "" {
		t.Fatalf("real SQL regression lost Query ID or sample provenance: %v", regression)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	for _, expected := range []string{eventID, fault.SampleID, baseline.SampleID, initial.SampleID,
		`"ruleId":"pg_sql_regression"`, `"type":"sql_fingerprint"`,
		`"relation":"compared_with"`} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("real SQL incident lacks %q: %s", expected, encoded)
		}
	}
	run(0.12)
	recovery := collect("recovery")
	var status string
	if err := pg.pool.QueryRow(ctx, `SELECT status FROM events WHERE id=$1`, eventID).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("SQL incident did not recover under one ID: %s %v", status, err)
	}
	var count int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*)::int FROM evidence WHERE event_id=$1
		AND kind='alert_recovered' AND payload_json->>'sampleId'=$2`, eventID, recovery.SampleID).
		Scan(&count); err != nil || count != 1 {
		t.Fatalf("recovery sample was not linked to the original SQL incident: %d %v", count, err)
	}
	if report, err := pg.EventReport(ctx, eventID, "md"); err != nil ||
		!strings.Contains(report, eventID) || !strings.Contains(report, recovery.SampleID) {
		t.Fatal("real SQL incident report lost its event or recovery sample", err)
	}
	t.Log(fmt.Sprintf("real PG18 SQL incident %s: ratio %.1f, fault %s, recovery %s",
		eventID, number(regression["maxRatio"]), fault.SampleID, recovery.SampleID))
}
