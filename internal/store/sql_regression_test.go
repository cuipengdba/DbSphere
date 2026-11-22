package store

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/trace"
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
	resetAt := time.Date(2026, 11, 19, 2, 0, 0, 0, time.UTC)
	oldest["resetAt"], previous["resetAt"], current["resetAt"] =
		resetAt.Format(time.RFC3339Nano), resetAt.Format(time.RFC3339Nano), &resetAt
	current["statements"] = []map[string]any{object(list(current["statements"])[0])}
	if mixed := sqlRegressionSignal(current, previous, oldest); mixed == nil || mixed["maxRatio"] != float64(10) {
		t.Fatalf("live time pointer and stored JSON reset timestamps did not compare: %v", mixed)
	}
	oldest["resetAt"], previous["resetAt"], current["resetAt"] = "reset-a", "reset-a", "reset-a"
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
	if os.Getenv("DBSPHERE_TEST_PG_TARGET_MAJOR") != "18" ||
		os.Getenv("DBSPHERE_TEST_REAL_SQL_REGRESSION") != "1" {
		t.Skip("real SQL regression drill requires the PostgreSQL 18 pg_stat_statements target")
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
		summary, _ := pg.SQLSummary(ctx, asset.ID)
		rows := list(object(object(summary)["performance"])["statements"])
		var leading any
		if len(rows) > 0 {
			leading = map[string]any{"queryId": object(rows[0])["queryId"],
				"regression": object(rows[0])["regression"]}
		}
		var faultRaw []byte
		_ = pg.pool.QueryRow(ctx, `SELECT metrics_json FROM samples WHERE id=$1`, fault.SampleID).Scan(&faultRaw)
		var faultMetrics map[string]any
		_ = json.Unmarshal(faultRaw, &faultMetrics)
		t.Fatalf("real SQL latency regression did not open an incident: %v; trend=%v; signal=%v",
			err, leading, faultMetrics["sqlRegression"])
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
	queryID := stringValue(object(regression["worst"])["queryId"])
	traceID := newTraceID(t)
	callStart := time.Now().UTC()
	if _, err := target.Exec(ctx, query, 1.2); err != nil {
		t.Fatal(err)
	}
	callEnd := time.Now().UTC()
	if imported, err := pg.SaveTraceSpans(ctx, []trace.Span{
		{AssetID: asset.ID, TraceID: traceID, SpanID: "1111111111111111",
			Service: "api", Kind: "service", Start: callStart, End: callEnd, Status: "ok"},
		{AssetID: asset.ID, TraceID: traceID, SpanID: "2222222222222222",
			ParentSpanID: "1111111111111111", Service: "checkout", Kind: "postgresql_client",
			QueryID: queryID, Start: callStart, End: callEnd, Status: "ok"},
	}); err != nil || imported != 2 {
		t.Fatalf("real slow call Trace did not import: %d %v", imported, err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	for _, expected := range []string{eventID, fault.SampleID, baseline.SampleID, initial.SampleID,
		`"ruleId":"pg_sql_regression"`, `"type":"sql_fingerprint"`,
		`"relation":"compared_with"`, `"relation":"calls_service"`,
		`"relation":"query_id_matches"`, `"sqlRegressionTraces":1`} {
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
	t.Logf("real PG18 SQL incident %s: ratio %.1f, fault %s, recovery %s",
		eventID, number(regression["maxRatio"]), fault.SampleID, recovery.SampleID)
}

// This optional drill reads actual PG18 EXPLAIN JSON and jsonlog entries. The
// plan baseline must really precede the incident by five minutes.
func TestRealPG18SQLRegressionLinksRealPlanAndSlowLog(t *testing.T) {
	if os.Getenv("DBSPHERE_TEST_REAL_SQL_PLAN") != "1" ||
		os.Getenv("DBSPHERE_TEST_PG_TARGET_MAJOR") != "18" {
		t.Skip("requires a dedicated PG18 target with pg_stat_statements and jsonlog")
	}
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	password := os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")
	port, err := strconv.Atoi(portText)
	if storeURL == "" || err != nil || password == "" {
		t.Fatal("PG18 store URL, target port and target password are required")
	}
	ctx := context.Background()
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Real PG18 SQL plan and log drill",
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
	for _, command := range []string{
		`CREATE TEMP TABLE dbsphere_plan_probe(id integer PRIMARY KEY, payload text)`,
		`INSERT INTO dbsphere_plan_probe SELECT n, repeat('x',100) FROM generate_series(1,10000) n`,
		`ANALYZE dbsphere_plan_probe`, `SET plan_cache_mode=force_custom_plan`,
		`SET enable_seqscan=off`,
	} {
		if _, err := target.Exec(ctx, command); err != nil {
			t.Fatal(err)
		}
	}
	const query = `SELECT pg_sleep($1::double precision), payload FROM dbsphere_plan_probe WHERE id=$2`
	const explain = `EXPLAIN (ANALYZE, VERBOSE, BUFFERS, FORMAT JSON) ` + query
	var priorPlan []byte
	priorAt := time.Now().UTC()
	if err := target.QueryRow(ctx, explain, 0.12, 1234).Scan(&priorPlan); err != nil {
		t.Fatal(err)
	}
	if _, err := target.Exec(ctx, query, 0.12, 1234); err != nil {
		t.Fatal(err)
	}
	collect := func(label string) CheckResult {
		t.Helper()
		result, err := pg.CheckAsset(ctx, asset.ID, key)
		if err != nil || result.Status != "ok" {
			t.Fatalf("%s: %+v %v", label, result, err)
		}
		return result
	}
	initial := collect("initial")
	// The SQL RCA contract requires an actual pre-incident plan, not a
	// backdated import timestamp. This wait is restricted to the optional drill.
	wait := time.Until(priorAt.Add(5*time.Minute + 5*time.Second))
	if wait > 0 {
		t.Logf("waiting %s for real pre-incident plan baseline", wait.Round(time.Second))
		time.Sleep(wait)
	}
	run := func(seconds float64) {
		t.Helper()
		for range 5 {
			if _, err := target.Exec(ctx, query, seconds, 1234); err != nil {
				t.Fatal(err)
			}
		}
	}
	run(0.12)
	baseline := collect("baseline")
	for _, command := range []string{`SET enable_seqscan=on`, `SET enable_indexscan=off`,
		`SET enable_bitmapscan=off`, `SET enable_indexonlyscan=off`} {
		if _, err := target.Exec(ctx, command); err != nil {
			t.Fatal(err)
		}
	}
	run(1.2)
	fault := collect("fault")
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1
		AND event_group='latency' AND status='open'`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal("SQL regression did not open an event", err)
	}
	var raw []byte
	if err := pg.pool.QueryRow(ctx, `SELECT metrics_json FROM samples WHERE id=$1`, fault.SampleID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var metrics map[string]any
	if err := json.Unmarshal(raw, &metrics); err != nil {
		t.Fatal(err)
	}
	queryID := stringValue(object(object(metrics["sqlRegression"])["worst"])["queryId"])
	if !slowQueryID.MatchString(queryID) {
		t.Fatalf("SQL alert lost Query ID: %s", raw)
	}
	if _, err := target.Exec(ctx, query, 1.2, 1234); err != nil {
		t.Fatal(err)
	}
	var currentPlan []byte
	currentAt := time.Now().UTC()
	if err := target.QueryRow(ctx, explain, 1.2, 1234).Scan(&currentPlan); err != nil {
		t.Fatal(err)
	}
	prior, err := pg.ImportSQLPlan(ctx, asset.ID, queryID, priorAt.Format(time.RFC3339Nano), priorPlan)
	if err != nil {
		t.Fatal(err)
	}
	current, err := pg.ImportSQLPlan(ctx, asset.ID, queryID, currentAt.Format(time.RFC3339Nano), currentPlan)
	if err != nil {
		t.Fatal(err)
	}
	var priorRecord, currentRecord map[string]any
	priorJSON, _ := json.Marshal(prior)
	currentJSON, _ := json.Marshal(current)
	if json.Unmarshal(priorJSON, &priorRecord) != nil || json.Unmarshal(currentJSON, &currentRecord) != nil ||
		priorRecord["shapeHash"] == currentRecord["shapeHash"] {
		t.Fatal("real PG18 planner did not change shape")
	}
	if currentRecord["database"] != "postgres" || object(currentRecord["summary"])["queryIdentity"] != "explain" {
		t.Fatal("the original PostgreSQL EXPLAIN Query Identifier and asset database were not verified")
	}
	var logContent string
	if err := target.QueryRow(ctx, `SELECT pg_read_file(pg_current_logfile())`).Scan(&logContent); err != nil {
		t.Fatal(err)
	}
	logs := make([]string, 0)
	for _, line := range strings.Split(logContent, "\n") {
		var record map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &record) != nil || strings.Trim(string(record["query_id"]), `"`) != queryID {
			continue
		}
		var message string
		_ = json.Unmarshal(record["message"], &message)
		if strings.HasPrefix(message, "duration: ") {
			logs = append(logs, line)
		}
	}
	if len(logs) == 0 {
		t.Fatal("PG18 jsonlog has no real slow statement with the alerted Query ID")
	}
	if _, err := pg.ImportSlowLogs(ctx, asset.ID, "jsonlog", strings.Join(logs, "\n")); err != nil {
		t.Fatal(err)
	}
	replay, err := pg.ImportSlowLogs(ctx, asset.ID, "jsonlog", " "+strings.Join(logs, "\n "))
	if err != nil || replay.(map[string]any)["imported"] != 0 {
		t.Fatalf("reformatted real log replay inserted duplicate records: %v %v", replay, err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	var candidate map[string]any
	for _, item := range object(detail.(map[string]any)["analysis"])["candidates"].([]map[string]any) {
		if item["code"] == "sql_plan_regression" {
			candidate = item
			break
		}
	}
	if candidate == nil || stringValue(object(candidate["detail"])["queryId"]) != queryID {
		t.Fatalf("real plan and jsonlog did not join exact SQL alert %s: %v", queryID, candidate)
	}
	encoded, _ := json.Marshal(detail)
	if !strings.Contains(string(encoded), `"relation":"compared_with"`) ||
		!strings.Contains(string(encoded), `"relation":"query_id_matches"`) ||
		!strings.Contains(string(encoded), baseline.SampleID) || !strings.Contains(string(encoded), initial.SampleID) {
		t.Fatal("event lost SQL plan, log or sample provenance")
	}
	t.Logf("real PG18 event %s: Query ID %s, fault %s, plans %s and %s, database postgres, jsonlog replay skipped", eventID,
		queryID, fault.SampleID, priorRecord["id"], currentRecord["id"])
}
