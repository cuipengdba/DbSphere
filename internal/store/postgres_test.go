package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/security"
)

func TestRealPostgreSQL18Connection(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	store, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	var version, partitions int
	if err := store.pool.QueryRow(context.Background(),
		"SELECT max(version) FROM dbsphere_schema_migrations").Scan(&version); err != nil || version < 15 {
		t.Fatalf("Go schema migration absent: %d %v", version, err)
	}
	if err := store.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM pg_partition_tree('samples'::regclass)").Scan(&partitions); err != nil || partitions < 3 {
		t.Fatalf("monthly sample partitions absent: %d %v", partitions, err)
	}
	hash, err := store.AdminPasswordHash(context.Background(), "first-bootstrap-password")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.AdminPasswordHash(context.Background(), "different-bootstrap-password")
	if err != nil || kept != hash {
		t.Fatal("existing administrator hash was replaced")
	}
	for _, path := range []string{"/api/assets", "/api/events", "/api/alerts", "/api/alert-rules",
		"/api/silences", "/api/notifications", "/api/storage", "/api/inspection-templates",
		"/api/inspection-schedules", "/api/inspection-runs"} {
		value, found, err := store.ReadCollection(context.Background(), path)
		if err != nil || !found {
			t.Fatalf("Go read API %s failed: %v", path, err)
		}
		encoded, err := json.Marshal(value)
		if err != nil || strings.Contains(string(encoded), "password_enc") || strings.Contains(string(encoded), "pooler_url_enc") {
			t.Fatalf("Go read API %s exposed secrets or invalid JSON: %v", path, err)
		}
	}
	var historicalEventID string
	if err := store.pool.QueryRow(context.Background(), "SELECT id::text FROM events ORDER BY started_at DESC LIMIT 1").Scan(&historicalEventID); err == nil {
		if detail, err := store.EventByID(context.Background(), historicalEventID); err != nil || detail == nil {
			t.Fatalf("existing PG18 event evidence is unreadable: %v", err)
		}
	}
	key := []byte("01234567890123456789012345678901")
	input := assets.Input{Name: "Go CRUD test", Host: "127.0.0.1", Port: 5432, Database: "postgres",
		Username: "dbsphere_monitor", Password: "fixture-secret", SSLMode: "disable", Runtime: "linux"}
	created, err := store.CreateAsset(context.Background(), input, key)
	if err != nil {
		t.Fatal(err)
	}
	var public struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(created, &public); err != nil {
		t.Fatal(err)
	}
	defer store.DeleteAsset(context.Background(), public.ID)
	if strings.Contains(string(created), "fixture-secret") || strings.Contains(string(created), "password_enc") {
		t.Fatal("asset API exposed encrypted or plaintext credential")
	}
	var oldCipher string
	if err := store.pool.QueryRow(context.Background(), "SELECT password_enc FROM assets WHERE id=$1", public.ID).Scan(&oldCipher); err != nil {
		t.Fatal(err)
	}
	plain, err := security.Decrypt(key, oldCipher)
	if err != nil || plain != "fixture-secret" {
		t.Fatal("asset secret did not survive PG18 storage")
	}
	input.Name = "Updated Go CRUD test"
	input.Password = ""
	updated, err := store.UpdateAsset(context.Background(), public.ID, input, key)
	if err != nil || !strings.Contains(string(updated), "Updated Go CRUD test") {
		t.Fatal("asset update failed", err)
	}
	var keptCipher string
	if err := store.pool.QueryRow(context.Background(), "SELECT password_enc FROM assets WHERE id=$1", public.ID).Scan(&keptCipher); err != nil || keptCipher != oldCipher {
		t.Fatal("blank edit replaced the existing credential")
	}
	deleted, err := store.DeleteAsset(context.Background(), public.ID)
	if err != nil || !deleted {
		t.Fatal("asset deletion failed", err)
	}
}

func TestViewerPasswordRotationAndDisableOnPostgreSQL18(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	store, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	defer store.ViewerPasswordHash(ctx, "")
	first, err := store.ViewerPasswordHash(ctx, "first-viewer-password")
	if err != nil || !security.VerifyPassword("first-viewer-password", first) {
		t.Fatal("viewer bootstrap failed", err)
	}
	kept, err := store.ViewerPasswordHash(ctx, "first-viewer-password")
	if err != nil || kept != first {
		t.Fatal("unchanged viewer password was rehashed", err)
	}
	rotated, err := store.ViewerPasswordHash(ctx, "rotated-viewer-password")
	if err != nil || rotated == first || !security.VerifyPassword("rotated-viewer-password", rotated) {
		t.Fatal("viewer password rotation failed", err)
	}
	if disabled, err := store.ViewerPasswordHash(ctx, ""); err != nil || disabled != "" {
		t.Fatal("viewer account disable failed", err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM settings WHERE key='viewer_password_hash'").Scan(&count); err != nil || count != 0 {
		t.Fatal("viewer credential remained in PG18 after disable", err)
	}
}

func TestOneEventIDAcrossFailureAndRecovery(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	if url == "" || portText == "" {
		t.Skip("PG18 store and PostgreSQL target are required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	input := assets.Input{Name: "Go event ID test", Host: "127.0.0.1", Port: port,
		Database: "postgres", Username: "postgres", Password: os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD"),
		SSLMode: "disable", Runtime: "linux"}
	created, err := pg.CreateAsset(context.Background(), input, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	due, err := pg.DueAssetIDs(context.Background(), time.Minute, 100)
	if err != nil || !slices.Contains(due, asset.ID) {
		t.Fatalf("new PostgreSQL asset was not scheduled for Go collection: %v %v", due, err)
	}
	first, err := pg.CheckAsset(context.Background(), asset.ID, key)
	if err != nil || first.Status != "ok" {
		t.Fatalf("initial PG14–18 sample failed: %v %v", first, err)
	}
	due, err = pg.DueAssetIDs(context.Background(), time.Minute, 100)
	if err != nil || slices.Contains(due, asset.ID) {
		t.Fatalf("freshly sampled asset was still due: %v %v", due, err)
	}
	input.Port = 1
	input.Password = ""
	if _, err := pg.UpdateAsset(context.Background(), asset.ID, input, key); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := pg.CheckAsset(context.Background(), asset.ID, key)
		if err != nil || result.Status != "error" {
			t.Fatalf("connection failure was not sampled: %v %v", result, err)
		}
	}
	var eventID string
	var count int
	if err := pg.pool.QueryRow(context.Background(), `SELECT id::text
		FROM events WHERE asset_id=$1 AND status='open' LIMIT 1`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal("failure event missing", err)
	}
	var diagnosticSampleID string
	metrics := map[string]any{
		"connections": 90, "max_connections": 100,
		"linux": map[string]any{"status": "ok"},
		"k8s": map[string]any{"status": "ok", "pod": map[string]any{"uid": "pod-test", "name": "pg-0"},
			"node": map[string]any{"name": "worker-1"},
			"pvcs": []any{map[string]any{"namespace": "database", "name": "pg-data"}}},
		"diagnostics": map[string]any{"lockChain": map[string]any{"status": "ok",
			"data": []any{map[string]any{"waitingPid": 101, "blockerPid": 202}}}},
		"sqlPerformance": map[string]any{"status": "ok", "statements": []any{
			map[string]any{"fingerprint": "test-fingerprint", "queryId": "42"}}},
	}
	metricsJSON, _ := json.Marshal(metrics)
	if err := pg.pool.QueryRow(context.Background(), `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
		VALUES(gen_random_uuid(),$1,now(),'ok',$2) RETURNING id`, asset.ID, metricsJSON).Scan(&diagnosticSampleID); err != nil {
		t.Fatal(err)
	}
	for _, metric := range []string{"pg.connection_utilization", "pg.blocked_sessions"} {
		payload, _ := json.Marshal(map[string]any{"sampleId": diagnosticSampleID, "metric": metric,
			"value": 90, "operator": ">=", "threshold": 85})
		if _, err := pg.pool.Exec(context.Background(), `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'alert_firing',now(),$2)`, eventID, payload); err != nil {
			t.Fatal(err)
		}
	}
	traceID := strings.Repeat("a", 32)
	for _, span := range []struct {
		id, parent, kind string
		backendPID       int
	}{
		{"1111111111111111", "", "service", 0},
		{"2222222222222222", "1111111111111111", "postgresql_client", 101},
	} {
		var parent any
		if span.parent != "" {
			parent = span.parent
		}
		if _, err := pg.pool.Exec(context.Background(), `INSERT INTO trace_spans
			(asset_id,trace_id,span_id,parent_span_id,service,kind,started_at,ended_at,status,backend_pid)
			VALUES($1,$2,$3,$4,'api',$5,now()-interval '1 second',now(),'ok',$6)`,
			asset.ID, traceID, span.id, parent, span.kind, optionalBackendPID(span.backendPID)); err != nil {
			t.Fatal(err)
		}
	}
	var slowLogID, planID string
	if err := pg.pool.QueryRow(context.Background(), `INSERT INTO slow_log_entries
		(id,asset_id,source_key,observed_at,duration_ms,query_id,source_format,imported_at)
		VALUES(gen_random_uuid(),$1,'go-rca-test',now(),1350,'42','csv',now()) RETURNING id`,
		asset.ID).Scan(&slowLogID); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(context.Background(), `INSERT INTO sql_plan_snapshots
		(id,asset_id,query_id,observed_at,source,shape_hash,summary_json,imported_at)
		VALUES(gen_random_uuid(),$1,'42',now(),'import','test-shape','{"nodeCount":1}',now()) RETURNING id`,
		asset.ID).Scan(&planID); err != nil {
		t.Fatal(err)
	}
	if err := pg.pool.QueryRow(context.Background(), `SELECT count(*)::int
		FROM events WHERE asset_id=$1 AND status='open'`, asset.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("repeated failures did not share one event: %q %d %v", eventID, count, err)
	}
	detail, err := pg.EventByID(context.Background(), eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(detail)
	if err != nil || !strings.Contains(string(encoded), "observed_in") || !strings.Contains(string(encoded), eventID) ||
		!strings.Contains(string(encoded), "connection_pressure") || !strings.Contains(string(encoded), "lock_chain") ||
		!strings.Contains(string(encoded), "trace_lock_pid_matched") ||
		!strings.Contains(string(encoded), "same_backend_pid") ||
		!strings.Contains(string(encoded), "blocked_by") || !strings.Contains(string(encoded), "sql_fingerprint") ||
		!strings.Contains(string(encoded), "scheduled_on") || !strings.Contains(string(encoded), "pvc:") ||
		!strings.Contains(string(encoded), "linux:") || !strings.Contains(string(encoded), "parent_of") ||
		!strings.Contains(string(encoded), "query_id_matches") || !strings.Contains(string(encoded), slowLogID) ||
		!strings.Contains(string(encoded), planID) {
		t.Fatal("same event ID did not expose its sample evidence graph", err)
	}
	for _, item := range []struct{ kind, id string }{{"slow-logs", slowLogID}, {"sql-plans", planID}} {
		if raw, err := pg.RawSQLEvidence(context.Background(), asset.ID, item.kind, item.id); err != nil || raw == nil {
			t.Fatalf("SQL evidence %s is unavailable: %v", item.kind, err)
		}
	}
	if rawTrace, err := pg.RawTrace(context.Background(), asset.ID, traceID); err != nil || rawTrace == nil {
		t.Fatal("trace evidence is unavailable", err)
	}
	if err := pg.RecordRCADecision(context.Background(), eventID, "unknown", "confirmed", "测试候选无证据"); !errors.Is(err, ErrInvalidRCADecision) {
		t.Fatalf("unsupported RCA candidate was accepted: %v", err)
	}
	if err := pg.RecordRCADecision(context.Background(), eventID, "lock_chain", "confirmed", "阻塞 PID 与采样匹配，需要继续核对业务请求"); err != nil {
		t.Fatal("RCA review was not stored", err)
	}
	if reviewed, err := pg.EventByID(context.Background(), eventID); err != nil {
		t.Fatal(err)
	} else if data, err := json.Marshal(reviewed); err != nil || !strings.Contains(string(data), "confirmed") {
		t.Fatal("RCA review did not appear in the same event", err)
	}
	if sample, err := pg.RawSample(context.Background(), asset.ID, first.SampleID); err != nil || sample == nil {
		t.Fatal("raw sample evidence is unavailable", err)
	}
	if acknowledged, err := pg.AcknowledgeEvent(context.Background(), eventID); err != nil || acknowledged == nil {
		t.Fatal("event acknowledgement failed", err)
	}
	input.Port = port
	if _, err := pg.UpdateAsset(context.Background(), asset.ID, input, key); err != nil {
		t.Fatal(err)
	}
	recovered, err := pg.CheckAsset(context.Background(), asset.ID, key)
	if err != nil || recovered.Status != "ok" {
		t.Fatalf("recovery sample failed: %v %v", recovered, err)
	}
	var status string
	if err := pg.pool.QueryRow(context.Background(), "SELECT status FROM events WHERE id=$1", eventID).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("event was not resolved under the same ID: %s %v", status, err)
	}
	if err := pg.pool.QueryRow(context.Background(), "SELECT count(*)::int FROM evidence WHERE event_id=$1", eventID).Scan(&count); err != nil || count < 7 {
		t.Fatalf("failure/recovery evidence did not share one ID: %d %v", count, err)
	}
	input.Runtime = "k8s"
	input.K8sCluster = "test"
	input.Namespace = "database"
	if _, err := pg.UpdateAsset(context.Background(), asset.ID, input, key); err != nil {
		t.Fatal(err)
	}
	k8sCheck, err := pg.CheckAsset(context.Background(), asset.ID, key)
	if err != nil || k8sCheck.Status != "error" {
		t.Fatal("Go collector incorrectly marked an unmapped Kubernetes asset healthy", k8sCheck, err)
	}
	if sample, err := pg.RawSample(context.Background(), asset.ID, k8sCheck.SampleID); err != nil ||
		!strings.Contains(fmt.Sprint(sample), "unmapped") {
		t.Fatal("Kubernetes mapping gap was not visible in the raw sample", err)
	}
}

func TestGoPgBouncerCombinedSampleInPG18(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	poolerURL := os.Getenv("DBSPHERE_TEST_POOLER_URL")
	if url == "" || portText == "" || poolerURL == "" {
		t.Skip("PG18, PostgreSQL target and PgBouncer are required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(context.Background(), assets.Input{Name: "Go PgBouncer test",
		Host: "127.0.0.1", Port: port, Database: "postgres", Username: "postgres",
		Password: os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD"), SSLMode: "disable", Runtime: "linux",
		PoolerURL: poolerURL}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	if strings.Contains(string(created), "dbsphere-monitor-test") {
		t.Fatal("PgBouncer credential leaked in asset response")
	}
	due, err := pg.DueAssetIDs(context.Background(), time.Minute, 100)
	if err != nil || !slices.Contains(due, asset.ID) {
		t.Fatal("PgBouncer asset was not scheduled", err)
	}
	checked, err := pg.CheckAsset(context.Background(), asset.ID, key)
	if err != nil || checked.Status != "ok" {
		t.Fatalf("combined PostgreSQL/PgBouncer collection failed: %+v %v", checked, err)
	}
	sample, err := pg.RawSample(context.Background(), asset.ID, checked.SampleID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(sample)
	if !strings.Contains(string(encoded), `"pgbouncer"`) || !strings.Contains(string(encoded), `"clientWaiting"`) ||
		strings.Contains(string(encoded), "dbsphere-monitor-test") {
		t.Fatalf("PgBouncer sample missing or credential leaked: %s", encoded)
	}
}

func TestConcurrentGoOpenDoesNotBlockEventReads(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	created, err := pg.CreateAsset(context.Background(), assets.Input{Name: "Go migration concurrency test",
		Host: "127.0.0.1", Port: 5432, Database: "postgres", Username: "postgres",
		Password: "test-secret", SSLMode: "disable", Runtime: "linux"},
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
	defer pg.DeleteAsset(context.Background(), asset.ID)
	if _, err := pg.recordCheck(context.Background(), asset.ID, nil, "availability", "PostgreSQL connection refused"); err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := pg.pool.QueryRow(context.Background(), `SELECT id::text FROM events WHERE asset_id=$1`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	problems := make(chan error, 10)
	var group sync.WaitGroup
	for range 5 {
		group.Add(1)
		go func() {
			defer group.Done()
			other, err := Open(ctx, url)
			if err != nil {
				problems <- err
				return
			}
			other.Close()
		}()
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := pg.EventByID(ctx, eventID); err != nil {
				problems <- err
			}
		}()
	}
	group.Wait()
	close(problems)
	for problem := range problems {
		t.Errorf("concurrent migration/read failed: %v", problem)
	}
}

func TestGoSQLIntervalAndRegressionFromPG18(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PG18 store is required")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(context.Background(), assets.Input{Name: "Go SQL trend test",
		Host: "127.0.0.1", Port: 5432, Database: "postgres", Username: "postgres",
		Password: "test-secret", SSLMode: "disable", Runtime: "linux"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	for index, snapshot := range []struct{ calls, total float64 }{
		{100, 1000}, {110, 1100}, {120, 2600},
	} {
		statement := map[string]any{"fingerprint": "1:2:3:1", "queryId": "3",
			"calls": snapshot.calls, "totalExecMs": snapshot.total, "rows": 0,
			"sharedBlocksHit": 0, "sharedBlocksRead": 0, "sharedBlocksWritten": 0,
			"tempBlocksRead": 0, "tempBlocksWritten": 0, "walBytes": 0}
		metrics, _ := json.Marshal(map[string]any{"sqlPerformance": map[string]any{
			"status": "ok", "source": "pg_stat_statements", "resetAt": "2026-11-17T00:00:00Z",
			"truncated": false, "hiddenQueryCount": 0, "statements": []any{statement}}})
		at := time.Now().UTC().Add(time.Duration(index-2) * time.Minute)
		if _, err := pg.pool.Exec(context.Background(), `INSERT INTO samples
			(id,asset_id,collected_at,status,metrics_json) VALUES(gen_random_uuid(),$1,$2,'ok',$3)`, asset.ID, at, metrics); err != nil {
			t.Fatal(err)
		}
	}
	value, err := pg.SQLSummary(context.Background(), asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if !strings.Contains(string(encoded), `"comparison":"interval"`) ||
		!strings.Contains(string(encoded), `"regression"`) || !strings.Contains(string(encoded), `"ratio":15`) {
		t.Fatalf("PG18 SQL samples did not produce interval regression: %s", encoded)
	}
}

func TestLinuxEvidenceSurvivesPostgresConnectionFailure(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	targetPortText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	exporterPortText := os.Getenv("DBSPHERE_TEST_NODE_EXPORTER_PORT")
	if storeURL == "" || targetPortText == "" || exporterPortText == "" {
		t.Skip("PG18 store, PostgreSQL target, and node_exporter are required")
	}
	targetPort, err := strconv.Atoi(targetPortText)
	if err != nil {
		t.Fatal(err)
	}
	exporterPort, err := strconv.Atoi(exporterPortText)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := Open(context.Background(), storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	input := assets.Input{Name: "Go Linux evidence test", Host: "127.0.0.1", Port: targetPort,
		Database: "postgres", Username: "postgres", Password: os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD"),
		SSLMode: "disable", Runtime: "linux", LinuxHost: "127.0.0.1",
		LinuxExporterPort: assets.OptionalPort(exporterPort)}
	created, err := pg.CreateAsset(context.Background(), input, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	due, err := pg.DueAssetIDs(context.Background(), time.Minute, 100)
	if err != nil || !slices.Contains(due, asset.ID) {
		t.Fatalf("Linux exporter asset was omitted from Go schedule: %v %v", due, err)
	}
	first, err := pg.CheckAsset(context.Background(), asset.ID, key)
	if err != nil || first.Status != "ok" {
		t.Fatalf("PostgreSQL plus Linux sample failed: %v %v", first, err)
	}
	input.Port, input.Password = 1, ""
	if _, err := pg.UpdateAsset(context.Background(), asset.ID, input, key); err != nil {
		t.Fatal(err)
	}
	failed, err := pg.CheckAsset(context.Background(), asset.ID, key)
	if err != nil || failed.Status != "error" {
		t.Fatalf("connection failure did not create cross-layer sample: %v %v", failed, err)
	}
	var eventID string
	if err := pg.pool.QueryRow(context.Background(), `SELECT id::text FROM events
		WHERE asset_id=$1 AND status='open' LIMIT 1`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	detail, err := pg.EventByID(context.Background(), eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	if !strings.Contains(string(encoded), "linux:") || !strings.Contains(string(encoded), failed.SampleID) {
		t.Fatalf("Linux host evidence was absent from PG failure event %s: %s", eventID, encoded)
	}
}
