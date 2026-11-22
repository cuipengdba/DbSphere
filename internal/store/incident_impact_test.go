package store

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/trace"
	"github.com/jackc/pgx/v5"
)

func TestObservedDependencyKeepsKubernetesClustersSeparate(t *testing.T) {
	metrics := map[string]any{"collector": map[string]any{"pgStatus": "ok"},
		"k8s": map[string]any{"cluster": "cluster-west"},
		"routing": map[string]any{"kind": "k8s_service", "entry": "pg.team.svc.cluster.local:5432",
			"status": "ready_pod_entry_unreachable", "probe": map[string]any{"status": "unreachable"},
			"service": map[string]any{"uid": "service-1"}}}
	west, _, _, _ := observedDependency(metrics)
	object(metrics["k8s"])["cluster"] = "cluster-east"
	east, _, _, _ := observedDependency(metrics)
	if west == "" || east == "" || west == east {
		t.Fatalf("separate Kubernetes scopes shared a dependency: %q %q", west, east)
	}
	object(metrics["collector"])["pgStatus"] = "availability"
	if dependency, _, _, _ := observedDependency(metrics); dependency != "" {
		t.Fatal("endpoint observation with a failing direct database must not group incidents")
	}
}

func TestRealHAProxySharedDependencyLinksAndSplitsOriginalEvents(t *testing.T) {
	target := os.Getenv("DBSPHERE_TEST_ROUTE_PG_URL")
	peer := os.Getenv("DBSPHERE_TEST_IMPACT_PEER_URL")
	entry := os.Getenv("DBSPHERE_TEST_HAPROXY_RECOVERY")
	if os.Getenv("DBSPHERE_TEST_IMPACT_PROXY") != "dbsphere-route-haproxy" || target == "" || peer == "" || entry == "" {
		t.Skip("dedicated CI HAProxy and two independent PostgreSQL targets are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, first, key := routeTestAsset(t, ctx, "")
	t.Setenv("DBSPHERE_TEST_ROUTE_PG_URL", peer)
	_, second, _ := routeTestAsset(t, ctx, "")
	t.Setenv("DBSPHERE_TEST_ROUTE_PG_URL", target)
	_, independent, _ := routeTestAsset(t, ctx, "")
	for asset, cluster := range map[string]string{first: "cluster-a", second: "cluster-b", independent: "cluster-c"} {
		if _, err := store.pool.Exec(ctx, `UPDATE assets SET cluster_name=$2,name=$2 WHERE id=$1`, asset, cluster); err != nil {
			t.Fatal(err)
		}
	}
	setup := func(asset, hostPort string) {
		t.Helper()
		if _, err := store.SetRouting(ctx, asset, RoutingInput{RoutingSettings: RoutingSettings{
			Kind: "haproxy", Entry: hostPort, ExpectedRole: "primary"}}, key); err != nil {
			t.Fatal(err)
		}
	}
	setup(first, entry)
	setup(second, entry)
	checkTestRoute(t, ctx, store, first, key, "ok")
	checkTestRoute(t, ctx, store, second, key, "ok")
	unbound, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	unrelatedEntry := unbound.Addr().String()
	unbound.Close()
	setup(independent, unrelatedEntry)
	proxy := func(action string) error {
		return exec.CommandContext(ctx, "docker", action, "dbsphere-route-haproxy").Run()
	}
	if err := proxy("stop"); err != nil {
		t.Fatal("stop isolated test proxy", err)
	}
	restarted := false
	defer func() {
		if !restarted {
			if err := proxy("start"); err != nil {
				t.Error("reopen isolated test proxy", err)
			}
		}
	}()
	firstSample, _ := checkTestRoute(t, ctx, store, first, key, "unreachable")
	secondSample, _ := checkTestRoute(t, ctx, store, second, key, "unreachable")
	otherSample, _ := checkTestRoute(t, ctx, store, independent, key, "unreachable")
	parent := checkRouteIncident(t, ctx, store, first, "route_entry_unreachable", firstSample)
	child := checkRouteIncident(t, ctx, store, second, "route_entry_unreachable", secondSample)
	other := checkRouteIncident(t, ctx, store, independent, "route_entry_unreachable", otherSample)
	host, portText, err := net.SplitHostPort(entry)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := pgx.ParseConfig(peer)
	if err != nil {
		t.Fatal("invalid test application connection settings")
	}
	cfg.Host, cfg.Port, cfg.ConnectTimeout = host, uint16(port), time.Second
	cfg.TLSConfig, cfg.Fallbacks = nil, nil
	start := time.Now().UTC()
	if conn, err := pgx.ConnectConfig(ctx, cfg); err == nil {
		conn.Close(ctx)
		t.Fatal("application unexpectedly reached the disabled shared proxy")
	}
	observed := trace.Span{AssetID: second, TraceID: "dcba1234abcd1234abcd1234abcd1234",
		SpanID: "dcba1234abcd1234", Service: "checkout", Kind: "postgresql_client", Start: start, End: time.Now().UTC(), Status: "error"}
	if written, err := store.SaveTraceSpans(ctx, []trace.Span{observed}); err != nil || written != 1 {
		t.Fatalf("record unsuccessful application call: %d %v", written, err)
	}
	read := func(id string, scope []string) map[string]any {
		t.Helper()
		impact, err := store.IncidentImpact(ctx, id, scope)
		if err != nil {
			t.Fatal(err)
		}
		return testRouteJSON(t, impact)
	}
	group := read(child, nil)
	members := list(group["members"])
	if group["primaryEventId"] != parent || len(members) != 2 || object(members[1])["eventId"] != child ||
		object(object(members[1])["dependency"])["sampleId"] != secondSample ||
		object(members[1])["source"] != "dependency" {
		t.Fatalf("real shared proxy did not link the distinct clusters: %+v", group)
	}
	services := list(object(members[1])["services"])
	if len(services) != 1 || object(services[0])["name"] != "checkout" {
		t.Fatalf("affected application service was not retained: %+v", services)
	}
	otherGroup := read(other, nil)
	scoped := read(child, []string{second})
	if otherGroup["primaryEventId"] != other || len(list(otherGroup["members"])) != 1 ||
		scoped["primaryEventId"] != child || len(list(scoped["members"])) != 1 || len(list(scoped["history"])) != 0 {
		t.Fatalf("unrelated or unauthorized incident was included: %+v %+v", otherGroup, scoped)
	}
	if _, err := store.IncidentImpact(ctx, parent, []string{second}); !errors.Is(err, ErrEventNotFound) {
		t.Fatal("scoped viewer read a different asset", err)
	}
	deliveries := func(event string) []string {
		t.Helper()
		var out []string
		err := store.pool.QueryRow(ctx, `SELECT array_agg(q.id::text ORDER BY q.id)
			FROM notification_outbox q JOIN evidence e ON e.id=q.evidence_id WHERE e.event_id=$1`, event).Scan(&out)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	previousDeliveries := deliveries(child)
	audit, err := store.StartAdminAudit(ctx, "POST", "/api/events/"+child+"/ack")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAdminAudit(ctx, audit, 200); err != nil {
		t.Fatal(err)
	}
	if err := proxy("start"); err != nil {
		t.Fatal("restart isolated test proxy", err)
	}
	restarted = true
	for i := 0; i < 20; i++ {
		conn, err := pgx.ConnectConfig(ctx, cfg)
		if err == nil {
			conn.Close(ctx)
			break
		}
		if i == 19 {
			t.Fatal("isolated proxy did not recover")
		}
		time.Sleep(200 * time.Millisecond)
	}
	recovered, _ := checkTestRoute(t, ctx, store, first, key, "ok")
	checkRouteRecovery(t, ctx, store, parent, recovered)
	members = list(read(parent, nil)["members"])
	if len(members) != 2 || object(members[0])["status"] != "resolved" || object(members[1])["status"] != "open" {
		t.Fatalf("recovery must remain scoped to each asset: %+v", members)
	}
	change := IncidentLinkInput{Action: "split", ChildEventID: child, Reason: "independent recovery tracking"}
	if err := store.UpdateIncidentLink(ctx, parent, change); err != nil {
		t.Fatal(err)
	}
	split := read(child, nil)
	history := list(split["history"])
	if split["primaryEventId"] != child || len(list(split["members"])) != 1 || len(history) != 1 ||
		object(history[0])["unlinkReason"] != change.Reason || len(previousDeliveries) == 0 ||
		!reflect.DeepEqual(previousDeliveries, deliveries(child)) {
		t.Fatalf("split discarded history or original notifications: %+v", split)
	}
	var retainedAudit string
	if err := store.pool.QueryRow(ctx, `SELECT event_id::text FROM admin_audit WHERE id=$1`, audit).Scan(&retainedAudit); err != nil ||
		retainedAudit != child {
		t.Fatal("original event audit did not survive the split", err)
	}
	if raw, err := store.RawSample(ctx, second, secondSample); err != nil || raw == nil {
		t.Fatal("original source sample could not be opened after split", err)
	}
	change.Action, change.Reason = "link", "operator verified common gateway"
	if err := store.UpdateIncidentLink(ctx, parent, change); err != nil {
		t.Fatal(err)
	}
	relinked := read(child, nil)
	if relinked["primaryEventId"] != parent || len(list(relinked["history"])) != 2 {
		t.Fatal("manual relink did not preserve both records", relinked)
	}
	t.Logf("shared proxy event=%s member=%s independent=%s source=%s recovery=%s", parent, child, other, secondSample, recovered)
}
