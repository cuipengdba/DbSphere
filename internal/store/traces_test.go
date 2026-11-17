package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/trace"
)

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
	identities, err := pg.TraceAssets(ctx)
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
		Start: now.Add(-time.Second), End: now, Status: "error"}
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
	if _, err := pg.pool.Exec(ctx, `UPDATE trace_spans SET ended_at=now()-interval '8 days'
		WHERE asset_id=$1 AND trace_id=$2`, asset.ID, span.TraceID); err != nil {
		t.Fatal(err)
	}
	if removed, err := pg.PruneTraceSpans(ctx, 7); err != nil || removed < 1 {
		t.Fatalf("expired trace was not pruned: %d %v", removed, err)
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
	identities, err := pg.TraceAssets(ctx)
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
}
