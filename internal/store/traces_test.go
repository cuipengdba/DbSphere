package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/trace"
	"github.com/jackc/pgx/v5"
)

func newTraceID(t *testing.T) string {
	t.Helper()
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(raw[:])
}

func TestGoTracePersistenceAndDedupPG18(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	ctx := context.Background()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Go trace test",
		Host: "pg-trace.internal", Port: 5432, Database: "orders", Username: "postgres",
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
	defer pg.DeleteAsset(ctx, asset.ID)
	identities, err := pg.TraceAssets(ctx, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, identity := range identities {
		if identity.ID == asset.ID && identity.Host == "pg-trace.internal" &&
			identity.Database == "orders" {
			found = true
		}
	}
	if !found {
		t.Fatal("trace asset identity was not listed")
	}
	now := time.Now().UTC()
	span := trace.Span{AssetID: asset.ID, TraceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SpanID: "1111111111111111", Service: "checkout", Kind: "postgresql_client",
		Start: now.Add(-time.Second), End: now, Status: "error", BackendPID: 301, QueryID: "-42"}
	if imported, err := pg.SaveTraceSpans(ctx, []trace.Span{span}); err != nil || imported != 1 {
		t.Fatalf("trace import failed: %d %v", imported, err)
	}
	if imported, err := pg.SaveTraceSpans(ctx, []trace.Span{span}); err != nil || imported != 0 {
		t.Fatalf("duplicate trace import created a second row: %d %v", imported, err)
	}
	value, err := pg.RawTrace(ctx, asset.ID, span.TraceID)
	if err != nil || value == nil {
		t.Fatalf("saved trace was not readable: %v", err)
	}
	encoded, _ := json.Marshal(value)
	if !strings.Contains(string(encoded), `"backendPid":301`) ||
		!strings.Contains(string(encoded), `"queryId":"-42"`) {
		t.Fatalf("validated backend PID and Query ID were not retained: %s", encoded)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE trace_spans SET ended_at=now()-interval '8 days'
		WHERE asset_id=$1 AND trace_id=$2`, asset.ID, span.TraceID); err != nil {
		t.Fatal(err)
	}
	if removed, err := pg.PruneTraceSpans(ctx, 7); err != nil || removed < 1 {
		t.Fatalf("expired trace was not pruned: %d %v", removed, err)
	}
}

func TestPgBouncerTraceRoutePersistsInOneEventGraphPG18(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if storeURL == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	ctx := context.Background()
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Pooler trace route", Host: "pg-pool-route.internal",
		Port: 5432, Database: "orders", Username: "monitor", Password: "database-secret",
		SSLMode: "disable", Runtime: "linux",
		PoolerURL: "postgres://monitor:pooler-secret@pool-route.internal:6432/pgbouncer"}, key)
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
	identities, err := pg.TraceAssets(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, identity := range identities {
		if identity.ID == asset.ID {
			found = identity.PoolerHost == "pool-route.internal" && identity.PoolerPort == 6432
			if strings.Contains(fmt.Sprintf("%+v", identity), "pooler-secret") {
				t.Fatal("PgBouncer credential leaked into Trace identity")
			}
		}
	}
	if !found {
		t.Fatal("decrypted PgBouncer endpoint was not available for Trace mapping")
	}
	now := time.Now().UTC()
	attribute := func(name, value string) map[string]any {
		return map[string]any{"key": name, "value": map[string]string{"stringValue": value}}
	}
	traceID := newTraceID(t)
	spansJSON, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{attribute("service.name", "checkout")}},
		"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{
			"traceId": traceID, "spanId": "1111111111111111", "kind": 3,
			"startTimeUnixNano": now.Add(-time.Second).UnixNano(),
			"endTimeUnixNano":   now.UnixNano(), "attributes": []any{
				attribute("db.system.name", "postgresql"),
				attribute("server.address", "pool-route.internal"),
				attribute("server.port", "6432"), attribute("db.namespace", "orders"),
				attribute("db.statement", "SELECT private_data")},
		}}}}}}})
	decoded, err := trace.Decode(spansJSON, identities, now)
	if err != nil || len(decoded.Spans) != 1 || decoded.Spans[0].Route != "pgbouncer" {
		t.Fatalf("pooler Trace was not uniquely mapped: %+v %v", decoded, err)
	}
	if _, err := pg.SaveTraceSpans(ctx, decoded.Spans); err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
		VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, asset.ID, now.Add(-time.Minute)).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,'alert','PgBouncer trace route','open',$3,'pooler_route_test')`,
		eventID, asset.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	for _, required := range []string{`"relation":"via_pooler"`, `"relation":"forwards_to"`,
		`"type":"pgbouncer"`} {
		if !strings.Contains(string(encoded), required) {
			t.Fatalf("event graph is missing %s: %s", required, encoded)
		}
	}
	raw, err := pg.RawTrace(ctx, asset.ID, traceID)
	if err != nil {
		t.Fatal(err)
	}
	rawJSON, _ := json.Marshal(raw)
	if !strings.Contains(string(rawJSON), `"route":"pgbouncer"`) ||
		strings.Contains(string(rawJSON), "private_data") || strings.Contains(string(rawJSON), "pooler-secret") {
		t.Fatalf("unsafe or incomplete raw Trace: %s", rawJSON)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE assets SET pooler_url_enc='corrupt-ciphertext' WHERE id=$1`, asset.ID); err != nil {
		t.Fatal(err)
	}
	identities, err = pg.TraceAssets(ctx, key)
	if err != nil {
		t.Fatalf("one invalid PgBouncer credential must not stop all Trace ingestion: %v", err)
	}
	for _, identity := range identities {
		if identity.ID == asset.ID {
			if identity.Host != "pg-pool-route.internal" || identity.PoolerHost != "" {
				t.Fatalf("invalid pooler URL must leave direct asset mapping available: %+v", identity)
			}
			return
		}
	}
	t.Fatal("asset disappeared after its PgBouncer credential became invalid")
}

func TestApplicationPodIdentityRequiresKubernetesUIDAndPersistsInEventPG18(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if storeURL == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	ctx := context.Background()
	var requests atomic.Int32
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/namespaces/apps/pods/checkout-0" ||
			r.Header.Get("Authorization") != "Bearer read-only-token" {
			t.Errorf("unexpected Kubernetes Pod lookup: %s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"metadata":{"namespace":"apps","name":"checkout-0","uid":"pod-uid-1",`+
			`"creationTimestamp":"2026-11-01T00:00:00Z"},"spec":{"nodeName":"worker-1",`+
			`"volumes":[{"persistentVolumeClaim":{"claimName":"checkout-data"}}]}}`)
	}))
	defer api.Close()
	ca := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: api.Certificate().Raw}))
	kubeconfig := fmt.Sprintf(`apiVersion: v1
clusters:
- name: cluster
  cluster: {server: %q, certificate-authority-data: %q}
contexts:
- name: test-context
  context: {cluster: cluster, user: reader}
users:
- name: reader
  user: {token: read-only-token}
current-context: test-context
`, api.URL, base64.StdEncoding.EncodeToString([]byte(ca)))
	configPath := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(configPath, []byte(kubeconfig), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBSPHERE_KUBECONFIG", configPath)
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "K8S application Pod trace",
		Host: "pg-app-k8s.internal", Port: 5432, Database: "orders", Username: "monitor",
		Password: "database-secret", SSLMode: "disable", Runtime: "k8s",
		K8sCluster: "test-context", Namespace: "db", Pod: "pg-0"}, key)
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
	identities, err := pg.TraceAssets(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	attribute := func(name, value string) map[string]any {
		return map[string]any{"key": name, "value": map[string]string{"stringValue": value}}
	}
	build := func(uid string) (string, []byte) {
		traceID := newTraceID(t)
		body, err := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": []any{attribute("service.name", "checkout"),
				attribute("k8s.namespace.name", "apps"), attribute("k8s.pod.name", "checkout-0"),
				attribute("k8s.pod.uid", uid)}},
			"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{
				"traceId": traceID, "spanId": "1111111111111111", "kind": 3,
				"startTimeUnixNano": now.Add(-time.Second).UnixNano(), "endTimeUnixNano": now.UnixNano(),
				"attributes": []any{attribute("db.system.name", "postgresql"),
					attribute("server.address", "pg-app-k8s.internal"),
					attribute("db.namespace", "orders"), attribute("db.statement", "SELECT private_data")},
			}}}}}}})
		if err != nil {
			t.Fatal(err)
		}
		return traceID, body
	}
	goodTraceID, goodBody := build("pod-uid-1")
	decoded, err := trace.Decode(goodBody, identities, now)
	if err != nil || len(decoded.Spans) != 1 || decoded.Spans[0].AppPodUID != "pod-uid-1" {
		t.Fatalf("OTLP application Pod attributes were not bounded and mapped: %+v %v", decoded, err)
	}
	if _, err := pg.SaveTraceSpans(ctx, decoded.Spans); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("expected one Kubernetes Pod GET, got %d", requests.Load())
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
		VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, asset.ID, now.Add(-time.Minute)).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,'alert','Application Pod identity','open',$3,'app_pod_test')`,
		eventID, asset.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	for _, marker := range []string{`"type":"app_pod"`, `"relation":"originated_from"`,
		`"relation":"scheduled_on"`, `"relation":"references_pvc"`,
		`"appPodReported":1`, `"appPodVerified":1`} {
		if !strings.Contains(string(encoded), marker) {
			t.Fatalf("verified application Pod chain is missing %s: %s", marker, encoded)
		}
	}
	raw, err := pg.RawTrace(ctx, asset.ID, goodTraceID)
	if err != nil {
		t.Fatal(err)
	}
	rawJSON, _ := json.Marshal(raw)
	if !strings.Contains(string(rawJSON), `"appPodVerified"`) ||
		strings.Contains(string(rawJSON), "private_data") ||
		strings.Contains(string(rawJSON), "database-secret") ||
		strings.Contains(string(rawJSON), "read-only-token") {
		t.Fatalf("verified application Pod trace leaked a secret or lost identity: %s", rawJSON)
	}
	_, repeatedBody := build("pod-uid-1")
	decoded, err = trace.Decode(repeatedBody, identities, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.SaveTraceSpans(ctx, decoded.Spans); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("repeated Pod UID bypassed the bounded API cache: %d GET requests", requests.Load())
	}
	badTraceID, badBody := build("replaced-uid")
	decoded, err = trace.Decode(badBody, identities, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.SaveTraceSpans(ctx, decoded.Spans); err != nil {
		t.Fatalf("Kubernetes UID mismatch stopped core Trace storage: %v", err)
	}
	bad, err := pg.RawTrace(ctx, asset.ID, badTraceID)
	if err != nil {
		t.Fatal(err)
	}
	badJSON, _ := json.Marshal(bad)
	if !strings.Contains(string(badJSON), `"appPodVerified":null`) {
		t.Fatalf("mismatched Pod UID was presented as verified: %s", badJSON)
	}
	detail, err = pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ = json.Marshal(detail)
	if !strings.Contains(string(encoded), `"appPodReported":3`) ||
		!strings.Contains(string(encoded), `"appPodVerified":2`) ||
		!strings.Contains(string(encoded), "未通过 Kubernetes API 核对") {
		t.Fatalf("unverified application Pod was not shown as a chain gap: %s", encoded)
	}
}

func TestRealPgBouncerEndpointMapsApplicationTracePG18(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	poolerURL := os.Getenv("DBSPHERE_TEST_POOLER_URL")
	pgPort := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	pgPassword := os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")
	if storeURL == "" || poolerURL == "" || pgPort == "" || pgPassword == "" {
		t.Skip("PG18 store, PostgreSQL target and PgBouncer are required")
	}
	ctx := context.Background()
	parsed, err := url.Parse(poolerURL)
	if err != nil {
		t.Fatal(err)
	}
	queryURL := *parsed
	queryURL.Path = "/postgres"
	client, err := pgx.Connect(ctx, queryURL.String())
	if err != nil {
		t.Fatalf("application could not reach PostgreSQL through PgBouncer: %v", err)
	}
	defer client.Close(ctx)
	var database string
	if err := client.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil || database != "postgres" {
		t.Fatalf("real PgBouncer query failed: %s %v", database, err)
	}
	port, err := strconv.Atoi(pgPort)
	if err != nil {
		t.Fatal(err)
	}
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Real PgBouncer application route",
		Host: "127.0.0.1", Port: port, Database: "postgres", Username: "postgres",
		Password: pgPassword, SSLMode: "disable", Runtime: "linux", PoolerURL: poolerURL}, key)
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
	identities, err := pg.TraceAssets(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	attribute := func(name, value string) map[string]any {
		return map[string]any{"key": name, "value": map[string]string{"stringValue": value}}
	}
	body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource": map[string]any{"attributes": []any{attribute("service.name", "pooler-app")}},
		"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{
			"traceId": newTraceID(t), "spanId": "1111111111111111", "kind": 3,
			"startTimeUnixNano": now.Add(-time.Second).UnixNano(),
			"endTimeUnixNano":   now.UnixNano(), "attributes": []any{
				attribute("db.system.name", "postgresql"),
				attribute("server.address", parsed.Hostname()),
				attribute("server.port", parsed.Port()),
				attribute("db.namespace", database)},
		}}}}}}})
	decoded, err := trace.Decode(body, identities, now)
	if err != nil || len(decoded.Spans) != 1 || decoded.Spans[0].AssetID != asset.ID ||
		decoded.Spans[0].Route != "pgbouncer" {
		t.Fatalf("real PgBouncer endpoint did not map to one asset: %+v %v", decoded, err)
	}
	if _, err := pg.SaveTraceSpans(ctx, decoded.Spans); err != nil {
		t.Fatal(err)
	}
}

func TestGoTraceLinksLateSDKParentAcrossOTLPRequests(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if storeURL == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	ctx := context.Background()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "SDK split export test",
		Host: "pg-sdk.internal", Port: 5432, Database: "orders", Username: "postgres",
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
	defer pg.DeleteAsset(ctx, asset.ID)
	identity := []trace.Asset{{ID: asset.ID, Host: "pg-sdk.internal", Port: 5432, Database: "orders"}}
	now := time.Now().UTC()
	traceID := newTraceID(t)
	attr := func(key, value string) map[string]any {
		return map[string]any{"key": key, "value": map[string]string{"stringValue": value}}
	}
	wrap := func(spans ...any) []byte {
		body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
			"resource":   map[string]any{"attributes": []any{attr("service.name", "sdk-checkout")}},
			"scopeSpans": []any{map[string]any{"spans": spans}}}}})
		return body
	}
	begin, end := now.Add(-2*time.Second).UnixNano(), now.UnixNano()
	db := map[string]any{"traceId": traceID, "spanId": "2222222222222222",
		"parentSpanId": "1111111111111111", "kind": 3,
		"startTimeUnixNano": begin, "endTimeUnixNano": end,
		"attributes": []any{attr("db.system.name", "postgresql"), attr("server.address", "pg-sdk.internal"),
			attr("db.namespace", "orders"), attr("db.statement", "SELECT secret")}}
	childBatch, err := trace.Decode(wrap(db), identity, now)
	if err != nil || len(childBatch.Spans) != 1 || len(childBatch.Candidates) != 0 {
		t.Fatalf("database child was not mapped: %+v %v", childBatch, err)
	}
	if imported, linked, err := pg.SaveTraceBatch(ctx, childBatch.Spans, childBatch.Candidates); err != nil || imported != 1 || linked != 0 {
		t.Fatalf("database child was not stored: %d %d %v", imported, linked, err)
	}
	parent := map[string]any{"traceId": traceID, "spanId": "1111111111111111",
		"parentSpanId": "3333333333333333", "startTimeUnixNano": begin, "endTimeUnixNano": end}
	grandparent := map[string]any{"traceId": traceID, "spanId": "3333333333333333",
		"startTimeUnixNano": begin, "endTimeUnixNano": end}
	unrelated := map[string]any{"traceId": traceID, "spanId": "4444444444444444",
		"startTimeUnixNano": begin, "endTimeUnixNano": end,
		"attributes": []any{attr("http.url", "https://secret.invalid/")}}
	lateBatch, err := trace.Decode(wrap(grandparent, unrelated, parent), identity, now)
	if err != nil || len(lateBatch.Spans) != 0 || len(lateBatch.Candidates) != 3 {
		t.Fatalf("late service candidates were lost: %+v %v", lateBatch, err)
	}
	if imported, linked, err := pg.SaveTraceBatch(ctx, lateBatch.Spans, lateBatch.Candidates); err != nil || imported != 2 || linked != 2 {
		t.Fatalf("late parent chain was not attached: %d %d %v", imported, linked, err)
	}
	value, err := pg.RawTrace(ctx, asset.ID, traceID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if !strings.Contains(string(encoded), "1111111111111111") ||
		!strings.Contains(string(encoded), "3333333333333333") ||
		strings.Contains(string(encoded), "4444444444444444") ||
		strings.Contains(string(encoded), "secret") {
		t.Fatalf("late chain leaked unrelated or sensitive spans: %s", encoded)
	}
}

func TestGoTraceLinksEarlySDKParentAndPrunesPendingPG18(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if storeURL == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	ctx := context.Background()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "SDK reordered export test",
		Host: "pg-reordered.internal", Port: 5432, Database: "orders", Username: "postgres",
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
	defer pg.DeleteAsset(ctx, asset.ID)
	now := time.Now().UTC()
	traceID := newTraceID(t)
	parent := trace.Span{TraceID: traceID, SpanID: "1111111111111111", Service: "sdk-checkout",
		Kind: "service", Start: now.Add(-time.Second), End: now, Status: "ok"}
	if imported, linked, err := pg.SaveTraceBatch(ctx, nil, []trace.Span{parent}); err != nil || imported != 0 || linked != 0 {
		t.Fatalf("early parent should only enter the short buffer: %d %d %v", imported, linked, err)
	}
	if value, err := pg.RawTrace(ctx, asset.ID, traceID); err != nil ||
		strings.Contains(fmt.Sprint(value), parent.SpanID) {
		t.Fatalf("unmapped service span became visible: %v %v", value, err)
	}
	child := trace.Span{AssetID: asset.ID, TraceID: traceID, SpanID: "2222222222222222",
		ParentSpanID: parent.SpanID, Service: "sdk-checkout", Kind: "postgresql_client",
		Start: now.Add(-time.Second), End: now, Status: "ok"}
	if imported, linked, err := pg.SaveTraceBatch(ctx, []trace.Span{child}, nil); err != nil || imported != 2 || linked != 0 {
		t.Fatalf("early parent did not join mapped child: %d %d %v", imported, linked, err)
	}
	value, err := pg.RawTrace(ctx, asset.ID, traceID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if !strings.Contains(string(encoded), parent.SpanID) || !strings.Contains(string(encoded), child.SpanID) {
		t.Fatalf("parent-first SDK trace was incomplete: %s", encoded)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE trace_pending_spans SET received_at=now()-interval '11 minutes'
		WHERE trace_id=$1 AND span_id=$2`, traceID, parent.SpanID); err != nil {
		t.Fatal(err)
	}
	if removed, err := pg.PrunePendingTraceSpans(ctx); err != nil || removed < 1 {
		t.Fatalf("expired pending span was not removed: %d %v", removed, err)
	}
}

func TestGoTraceReconcilesConcurrentCollectorBatchCommits(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if storeURL == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	ctx := context.Background()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Concurrent Collector batch test",
		Host: "pg-concurrent.internal", Port: 5432, Database: "orders", Username: "postgres",
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
	defer pg.DeleteAsset(ctx, asset.ID)
	now := time.Now().UTC()
	traceID := newTraceID(t)
	parentID, childID := "1111111111111111", "2222222222222222"
	if _, err := pg.pool.Exec(ctx, `INSERT INTO trace_pending_spans
		(trace_id,span_id,service,started_at,ended_at,status) VALUES($1,$2,'sdk', $3,$4,'ok')`,
		traceID, parentID, now.Add(-time.Second), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO trace_spans
		(asset_id,trace_id,span_id,parent_span_id,service,kind,started_at,ended_at,status)
		VALUES($1,$2,$3,$4,'sdk','postgresql_client',$5,$6,'ok')`,
		asset.ID, traceID, childID, parentID, now.Add(-time.Second), now); err != nil {
		t.Fatal(err)
	}
	// Both rows are committed, as when two Collector exports finish together.
	// A new request in that trace must reconcile the previously missed edge.
	unrelated := trace.Span{TraceID: traceID, SpanID: "4444444444444444", Service: "sdk",
		Kind: "service", Start: now.Add(-time.Second), End: now, Status: "ok"}
	if imported, linked, err := pg.SaveTraceBatch(ctx, nil, []trace.Span{unrelated}); err != nil || imported != 1 || linked != 0 {
		t.Fatalf("committed parent/child race was not reconciled: %d %d %v", imported, linked, err)
	}
	value, err := pg.RawTrace(ctx, asset.ID, traceID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(value)
	if !strings.Contains(string(encoded), parentID) || strings.Contains(string(encoded), unrelated.SpanID) {
		t.Fatalf("reconciliation exposed an unrelated service span: %s", encoded)
	}
}

func TestGoOTLPTraceConnectsToExistingIncidentID(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	pg, err := Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	ctx := context.Background()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Go linked trace test",
		Host: "pg-linked.internal", Port: 5432, Database: "orders", Username: "postgres",
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
	defer pg.DeleteAsset(ctx, asset.ID)
	started := time.Now().UTC().Add(-3 * time.Minute)
	pressure := map[string]any{"version_num": 180000, "connections": 90, "max_connections": 100}
	for _, at := range []time.Time{started, started.Add(121 * time.Second)} {
		if _, err := pg.recordCheckAt(ctx, asset.ID, pressure, "", "", at); err != nil {
			t.Fatal(err)
		}
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1 AND event_group='capacity'`,
		asset.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	startNano := started.Add(122 * time.Second).UnixNano()
	endNano := started.Add(123 * time.Second).UnixNano()
	traceID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	body := fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeSpans":[{"spans":[{"traceId":"%s","spanId":"1111111111111111","startTimeUnixNano":"%d","endTimeUnixNano":"%d"},{"traceId":"%s","spanId":"2222222222222222","parentSpanId":"1111111111111111","kind":3,"startTimeUnixNano":"%d","endTimeUnixNano":"%d","attributes":[{"key":"db.system.name","value":{"stringValue":"postgresql"}},{"key":"server.address","value":{"stringValue":"pg-linked.internal"}},{"key":"db.namespace","value":{"stringValue":"orders"}}]}]}]}]}`,
		traceID, startNano, endNano, traceID, startNano, endNano)
	identities, err := pg.TraceAssets(ctx, []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := trace.Decode([]byte(body), identities, time.Now().UTC())
	if err != nil || len(parsed.Spans) != 2 || parsed.Rejected != 0 {
		t.Fatalf("OTLP trace did not map: %+v %v", parsed, err)
	}
	if imported, err := pg.SaveTraceSpans(ctx, parsed.Spans); err != nil || imported != 2 {
		t.Fatalf("OTLP trace not persisted: %d %v", imported, err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	if !strings.Contains(string(encoded), eventID) || !strings.Contains(string(encoded), "parent_of") ||
		!strings.Contains(string(encoded), "db_client_span") || !strings.Contains(string(encoded), "checkout") {
		t.Fatalf("existing incident did not include application-to-PG trace: %s", encoded)
	}
	var impactView struct {
		Analysis struct {
			TraceCoverage struct {
				UnresolvedParentSpans int                  `json:"unresolvedParentSpans"`
				ServiceImpact         []serviceTraceImpact `json:"serviceImpact"`
			} `json:"traceCoverage"`
		} `json:"analysis"`
	}
	if err := json.Unmarshal(encoded, &impactView); err != nil || len(impactView.Analysis.TraceCoverage.ServiceImpact) != 1 ||
		impactView.Analysis.TraceCoverage.ServiceImpact[0].Service != "checkout" ||
		impactView.Analysis.TraceCoverage.ServiceImpact[0].Spans != 2 ||
		impactView.Analysis.TraceCoverage.ServiceImpact[0].DatabaseSpans != 1 ||
		impactView.Analysis.TraceCoverage.ServiceImpact[0].ObservedTraces != 1 ||
		impactView.Analysis.TraceCoverage.ServiceImpact[0].CorrelatedTraces != 0 {
		t.Fatalf("observed service impact was not counted: %+v %v", impactView, err)
	}
	if report, err := pg.EventReport(ctx, eventID, "md"); err != nil ||
		!strings.Contains(report, "checkout：已观测 Span 2 条，数据库调用 1 条") {
		t.Fatal("incident report lost observed application impact", err)
	}
	if prompt, _, err := pg.EventAIEvidence(ctx, eventID); err != nil || strings.Contains(prompt, "checkout") {
		t.Fatal("AI evidence exposed application service name", err)
	}
	if _, err := pg.pool.Exec(ctx, `DELETE FROM trace_spans WHERE asset_id=$1 AND span_id=$2`,
		asset.ID, "1111111111111111"); err != nil {
		t.Fatal(err)
	}
	orphaned, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	orphanJSON, _ := json.Marshal(orphaned)
	if err := json.Unmarshal(orphanJSON, &impactView); err != nil ||
		impactView.Analysis.TraceCoverage.UnresolvedParentSpans != 1 {
		t.Fatalf("missing parent span was not marked as a chain gap: %s %v", orphanJSON, err)
	}
	if report, err := pg.EventReport(ctx, eventID, "md"); err != nil ||
		!strings.Contains(report, "链路断点：1 条 Span") {
		t.Fatal("incident report omitted the missing parent span", err)
	}
}

func TestRealPostgreSQLLockPIDConnectsApplicationTraceToOneEvent(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	password := os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")
	if storeURL == "" || portText == "" || password == "" {
		t.Skip("PG18 store and PostgreSQL target are required")
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
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Go real PID trace test", Host: "localhost",
		Port: port, Database: "postgres", Username: "postgres", Password: password,
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
	targetURL := url.URL{Scheme: "postgres", Host: "127.0.0.1:" + portText,
		Path: "/postgres", User: url.UserPassword("postgres", password), RawQuery: "sslmode=disable"}
	blocker, err := pgx.Connect(ctx, targetURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(ctx)
	waiter, err := pgx.Connect(ctx, targetURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer waiter.Close(ctx)
	var blockerPID, waitingPID int
	if err := blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if err := waiter.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&waitingPID); err != nil {
		t.Fatal(err)
	}
	lockKey := time.Now().UnixNano()
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	defer blocker.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey)
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		_, _ = waiter.Exec(waitCtx, "SELECT pg_advisory_lock($1)", lockKey)
	}()
	blocked := false
	for range 40 {
		var count int
		if err := blocker.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1))", waitingPID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			blocked = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !blocked {
		t.Fatal("real PostgreSQL waiter never blocked")
	}
	spanStart := time.Now().UTC().Add(-time.Second)
	result, err := pg.CheckAsset(ctx, asset.ID, key)
	if err != nil || result.Status != "ok" {
		t.Fatalf("real PG lock collection failed: %+v %v", result, err)
	}
	spanEnd := time.Now().UTC()
	var metricsJSON []byte
	if err := pg.pool.QueryRow(ctx, "SELECT metrics_json FROM samples WHERE id=$1", result.SampleID).Scan(&metricsJSON); err != nil {
		t.Fatal(err)
	}
	var metrics map[string]any
	if err := json.Unmarshal(metricsJSON, &metrics); err != nil {
		t.Fatal(err)
	}
	chain := object(object(metrics["diagnostics"])["lockChain"])
	foundPair := false
	for _, raw := range list(chain["data"]) {
		row := object(raw)
		if number(row["waitingPid"]) == float64(waitingPID) && number(row["blockerPid"]) == float64(blockerPID) {
			foundPair = true
		}
	}
	if !foundPair {
		t.Fatalf("real lock pair was absent from PG sample: %s", metricsJSON)
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
		VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, asset.ID, spanStart).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,'alert','Real lock wait','open',$3,'trace_pid_test')`, eventID, asset.ID, spanStart); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"sampleId": result.SampleID, "metric": "pg.blocked_sessions",
		"value": 1, "operator": ">=", "threshold": 1})
	if _, err := pg.pool.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'alert_firing',$2,$3)`, eventID, result.CollectedAt, payload); err != nil {
		t.Fatal(err)
	}
	attribute := func(name, value string) map[string]any {
		return map[string]any{"key": name, "value": map[string]string{"stringValue": value}}
	}
	traceID := strings.Repeat("e", 32)
	parent := map[string]any{"traceId": traceID, "spanId": "1111111111111111",
		"startTimeUnixNano": spanStart.UnixNano(), "endTimeUnixNano": spanEnd.UnixNano()}
	dbSpan := map[string]any{"traceId": traceID, "spanId": "2222222222222222",
		"parentSpanId": "1111111111111111", "kind": 3,
		"startTimeUnixNano": spanStart.UnixNano(), "endTimeUnixNano": spanEnd.UnixNano(),
		"attributes": []any{attribute("db.system.name", "postgresql"),
			attribute("server.address", "localhost"), attribute("server.port", portText),
			attribute("db.namespace", "postgres"),
			attribute("dbsphere.pg.backend_pid", strconv.Itoa(waitingPID))}}
	body, _ := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []any{attribute("service.name", "checkout")}},
		"scopeSpans": []any{map[string]any{"spans": []any{parent, dbSpan}}}}}})
	identities, err := pg.TraceAssets(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := trace.Decode(body, identities, time.Now().UTC())
	if err != nil || len(parsed.Spans) != 2 {
		t.Fatalf("real PID trace did not map: %+v %v", parsed, err)
	}
	if imported, err := pg.SaveTraceSpans(ctx, parsed.Spans); err != nil || imported != 2 {
		t.Fatalf("real PID trace was not stored: %d %v", imported, err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	if !strings.Contains(string(encoded), eventID) ||
		!strings.Contains(string(encoded), `"code":"trace_lock_pid_matched"`) ||
		!strings.Contains(string(encoded), `"relation":"same_backend_pid"`) ||
		!strings.Contains(string(encoded), `"lockWaitTraces":1`) ||
		!strings.Contains(string(encoded), `"correlatedTraces":1`) {
		t.Fatalf("real PG lock was not linked to one application event: %s", encoded)
	}
	if report, err := pg.EventReport(ctx, eventID, "md"); err != nil ||
		!strings.Contains(report, "匹配条件：PG 等待 PID") {
		t.Fatal("incident report lost exact PID match conditions", err)
	}
	cancel()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("real lock waiter did not stop")
	}
}
