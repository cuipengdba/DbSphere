package store

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/jackc/pgx/v5"
)

func routeTestAsset(t *testing.T, ctx context.Context, poolerURL string) (*Postgres, string, []byte) {
	t.Helper()
	dsn := os.Getenv("DBSPHERE_TEST_PG18_URL")
	target := os.Getenv("DBSPHERE_TEST_ROUTE_PG_URL")
	if dsn == "" || target == "" {
		t.Skip("independent PG18 store and route target are required")
	}
	config, err := pgx.ParseConfig(target)
	if err != nil {
		t.Fatalf("invalid route test target settings")
	}
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: t.Name(), Host: config.Host, Port: int(config.Port),
		Database: config.Database, Username: config.User, Password: config.Password, SSLMode: "disable",
		Runtime: "linux", PoolerURL: poolerURL}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct{ ID string }
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pg.DeleteAsset(context.Background(), asset.ID) })
	return pg, asset.ID, key
}

func checkTestRoute(t *testing.T, ctx context.Context, pg *Postgres, id string, key []byte, want string) (string, map[string]any) {
	t.Helper()
	checked, err := pg.CheckAsset(ctx, id, key)
	if err != nil || checked.Status != "ok" {
		t.Fatalf("direct database should remain available: %+v %v", checked, err)
	}
	raw, err := pg.RawSample(ctx, id, checked.SampleID)
	if err != nil || raw == nil {
		t.Fatal("missing raw route sample", err)
	}
	metrics := testRouteJSON(t, raw)["sample"]
	route := object(object(object(metrics)["metrics"])["routing"])
	if route["status"] != want {
		t.Fatalf("sample %s: expected %s, got %+v", checked.SampleID, want, route)
	}
	t.Logf("sample=%s route=%s entry=%s", checked.SampleID, want, route["entry"])
	return checked.SampleID, route
}

func testRouteJSON(t *testing.T, value any) map[string]any {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "dbsphere-monitor-test") || strings.Contains(string(body), "route-stats-test") {
		t.Fatal("route credentials leaked")
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func checkRouteIncident(t *testing.T, ctx context.Context, pg *Postgres, asset, rootCode, sampleID string) string {
	t.Helper()
	var id string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1 AND status='open'
		AND kind='alert_incident' ORDER BY started_at DESC LIMIT 1`, asset).Scan(&id); err != nil {
		t.Fatal("no open route incident", err)
	}
	result, err := pg.EventByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	analysis := object(testRouteJSON(t, result)["analysis"])
	for _, item := range list(analysis["candidates"]) {
		candidate := object(item)
		if candidate["code"] != rootCode {
			continue
		}
		for _, support := range list(candidate["supports"]) {
			if support == "sample:"+sampleID {
				t.Logf("event=%s candidate=%s source=%s", id, rootCode, sampleID)
				return id
			}
		}
	}
	t.Fatalf("incident %s missing candidate %s or sample %s: %+v", id, rootCode, sampleID, analysis["candidates"])
	return ""
}

func checkRouteRecovery(t *testing.T, ctx context.Context, pg *Postgres, eventID, sampleID string) {
	t.Helper()
	result, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	event := object(testRouteJSON(t, result)["event"])
	if event["status"] != "resolved" || event["id"] != eventID {
		t.Fatalf("original event was not resolved: %s %v", eventID, event["status"])
	}
	for _, raw := range list(event["evidence"]) {
		item := object(raw)
		if item["kind"] == "alert_recovered" && object(item["payload"])["sampleId"] == sampleID {
			t.Logf("event=%s recovered sample=%s", eventID, sampleID)
			return
		}
	}
	t.Fatal("original event has no recovery evidence from sample", sampleID)
}

func TestRealHAProxyRouteMismatchKeepsOneEvent(t *testing.T) {
	entry := os.Getenv("DBSPHERE_TEST_HAPROXY_ROUTE")
	recovery := os.Getenv("DBSPHERE_TEST_HAPROXY_RECOVERY")
	stats := os.Getenv("DBSPHERE_TEST_HAPROXY_STATS")
	if entry == "" || recovery == "" || stats == "" {
		t.Skip("real HAProxy stats and two frontends are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	pg, id, key := routeTestAsset(t, ctx, "")
	config := RoutingInput{RoutingSettings: RoutingSettings{Kind: "haproxy", Entry: entry, ExpectedRole: "primary",
		MatchDirect: true, HAProxyBackend: "pg_wrong"}, HAProxyStatsURL: stats}
	settings, err := pg.SetRouting(ctx, id, config, key)
	if err != nil || !settings.StatsConfigured {
		t.Fatal("could not configure HAProxy route", err)
	}
	observed, err := pg.ReadRouting(ctx, id)
	if err != nil || observed != settings {
		t.Fatal("could not read configured route", err)
	}
	testRouteJSON(t, observed)
	faultID, fault := checkTestRoute(t, ctx, pg, id, key, "misrouted")
	proxy := object(fault["haproxy"])
	if proxy["status"] != "ok" || proxy["backend"] != "pg_wrong" || fault["roleMismatch"] != false ||
		fault["differentBackend"] != true || len(list(proxy["rows"])) < 2 {
		t.Fatalf("HAProxy statistics and two primary identities missing: %+v", fault)
	}
	event := checkRouteIncident(t, ctx, pg, id, "route_backend_mismatch", faultID)
	config.Entry, config.HAProxyBackend, config.HAProxyStatsURL = recovery, "pg_correct", ""
	if _, err := pg.SetRouting(ctx, id, config, key); err != nil {
		t.Fatal(err)
	}
	recoveredID, result := checkTestRoute(t, ctx, pg, id, key, "ok")
	if object(result["haproxy"])["status"] != "ok" {
		t.Fatal("stats were not retained")
	}
	checkRouteRecovery(t, ctx, pg, event, recoveredID)
}

func TestRealPgBouncerPoolExhaustionKeepsOneEvent(t *testing.T) {
	admin := os.Getenv("DBSPHERE_TEST_ROUTE_POOLER_URL")
	if admin == "" {
		t.Skip("PgBouncer with pool size 1 is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	adminURL, err := url.Parse(admin)
	if err != nil {
		t.Fatal("invalid test pooler URL")
	}
	business := *adminURL
	business.Path = "/postgres"
	pg, id, key := routeTestAsset(t, ctx, admin)
	zero := 0
	if _, err := pg.UpdateAlertRule(ctx, "pgbouncer_waiting", AlertRulePatch{DurationSeconds: &zero}); err != nil {
		t.Fatal(err)
	}
	route := RoutingInput{RoutingSettings: RoutingSettings{Kind: "pgbouncer", Entry: business.Host, ExpectedRole: "primary", MatchDirect: true}}
	if _, err := pg.SetRouting(ctx, id, route, key); err != nil {
		t.Fatal(err)
	}
	checkTestRoute(t, ctx, pg, id, key, "ok")
	held, err := pgx.Connect(ctx, business.String())
	if err != nil {
		t.Fatal("open pool client", err)
	}
	defer held.Close(ctx)
	tx, err := held.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	queued := make(chan error, 1)
	go func() {
		conn, err := pgx.Connect(ctx, business.String())
		if err == nil {
			_, err = conn.Exec(ctx, "SELECT 1")
			conn.Close(ctx)
		}
		queued <- err
	}()
	waitUntil := time.Now().Add(10 * time.Second)
	for {
		stats, err := collector.PgBouncer(ctx, admin, "postgres")
		if err != nil {
			t.Fatal(err)
		}
		rows, _ := stats["pools"].([]map[string]any)
		if len(rows) == 1 && rows[0]["clientWaiting"] == float64(1) && rows[0]["serverIdle"] == float64(0) {
			break
		}
		if time.Now().After(waitUntil) {
			t.Fatalf("real pool did not saturate: %+v", stats)
		}
		time.Sleep(100 * time.Millisecond)
	}
	sample, fault := checkTestRoute(t, ctx, pg, id, key, "pool_exhausted")
	if object(fault["pooler"])["source"] != "PgBouncer SHOW POOLS/STATS" {
		t.Fatal("raw source missing")
	}
	eventID := checkRouteIncident(t, ctx, pg, id, "pooler_exhausted", sample)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-queued; err != nil {
		t.Fatal(err)
	}
	recovered, _ := checkTestRoute(t, ctx, pg, id, key, "ok")
	checkRouteRecovery(t, ctx, pg, eventID, recovered)
}
