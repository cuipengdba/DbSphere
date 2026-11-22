package store

import (
	"context"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRealPG18ChangeTimelineSeparatesObservedAndOperator(t *testing.T) {
	adminURL := os.Getenv("DBSPHERE_TEST_CHANGE_TARGET_ADMIN_URL")
	entry := os.Getenv("DBSPHERE_TEST_HAPROXY_ROUTE")
	if adminURL == "" || entry == "" {
		t.Skip("isolated PG18 target and a deliberately misrouted HAProxy entry are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	pg, asset, key := routeTestAsset(t, ctx, "")
	target, err := pgx.ParseConfig(os.Getenv("DBSPHERE_TEST_ROUTE_PG_URL"))
	if err != nil {
		t.Fatal("invalid dedicated PostgreSQL target")
	}
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal("open dedicated PG18 test administrator", err)
	}
	defer admin.Close(ctx)
	baseline, err := pg.CheckAsset(ctx, asset, key)
	if err != nil || baseline.Status != "ok" {
		t.Fatalf("collect the unchanged PG18 target: %+v %v", baseline, err)
	}
	statement := "ALTER ROLE " + pgx.Identifier{target.User}.Sanitize() + " IN DATABASE " +
		pgx.Identifier{target.Database}.Sanitize()
	if _, err := admin.Exec(ctx, statement+" SET log_min_duration_statement TO '2718ms'"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.Exec(ctx, statement+" RESET log_min_duration_statement"); err != nil {
			t.Error("reset dedicated test role parameter", err)
		}
	}()
	manual := ChangeInput{Kind: "deployment", Label: "Synthetic application deployment",
		OccurredAt: time.Now().UTC().Format(time.RFC3339Nano)}
	deploy, err := pg.AddChange(ctx, asset, manual)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := pg.AddChange(ctx, asset, manual)
	if err != nil || object(deploy)["id"] != object(retry)["id"] || object(deploy)["recordedAt"] != object(retry)["recordedAt"] {
		t.Fatal("retry produced a second change or modified the timestamp", err)
	}
	if _, err := pg.AddChange(ctx, "00000000-0000-4000-8000-000000000001", manual); !errors.Is(err, ErrAssetNotFound) {
		t.Fatal("unknown asset accepted an external change", err)
	}
	route := RoutingInput{RoutingSettings: RoutingSettings{Kind: "haproxy", Entry: entry, ExpectedRole: "primary", MatchDirect: true}}
	if _, err := pg.SetRouting(ctx, asset, route, key); err != nil {
		t.Fatal(err)
	}
	afterID, _ := checkTestRoute(t, ctx, pg, asset, key, "misrouted")
	eventID := checkRouteIncident(t, ctx, pg, asset, "route_backend_mismatch", afterID)
	params := url.Values{"from": {baseline.CollectedAt.Add(-time.Minute).Format(time.RFC3339Nano)},
		"to": {time.Now().UTC().Add(time.Second).Format(time.RFC3339Nano)}, "changes": {"true"}}
	result, err := pg.ReadAssetSection(ctx, asset, "changes", params)
	if err != nil {
		t.Fatal(err)
	}
	rows := list(testRouteJSON(t, result)["changes"])
	if len(rows) != 2 {
		t.Fatalf("expected one observed parameter change and one manual record: %+v", rows)
	}
	for _, raw := range rows {
		row := object(raw)
		if row["assetId"] != asset || object(row["before"])["id"] != baseline.SampleID ||
			object(row["after"])["id"] != afterID || row["recordedAt"] == nil {
			t.Fatalf("change sample ownership or timing mismatch: %+v", row)
		}
		switch row["source"] {
		case "pg_settings":
			fields := list(row["fields"])
			if len(fields) != 1 || object(fields[0])["name"] != "log_min_duration_statement" ||
				object(fields[0])["after"] != "2718 ms" || row["kind"] != "configuration" {
				t.Fatalf("actual PostgreSQL configuration change missing: %+v", row)
			}
			t.Logf("observed change=%s field=%v before=%s after=%s", row["id"], fields[0], baseline.SampleID, afterID)
		case "operator":
			recorded, _ := time.Parse(time.RFC3339Nano, stringValue(row["recordedAt"]))
			occurred, _ := time.Parse(time.RFC3339Nano, stringValue(row["occurredAt"]))
			if row["id"] != object(deploy)["id"] || len(list(row["fields"])) != 0 ||
				occurred.IsZero() || recorded.IsZero() || recorded.Before(occurred) {
				t.Fatalf("operator entry needs separate occurrence and recording times: %+v", row)
			}
			t.Logf("operator deployment=%s occurredAt=%s recordedAt=%s", row["id"], row["occurredAt"], row["recordedAt"])
		default:
			t.Fatalf("unexpected change source: %+v", row)
		}
	}
	contextView, err := pg.monitoringContext(ctx, asset, params)
	if err != nil {
		t.Fatal(err)
	}
	events := list(testRouteJSON(t, contextView)["events"])
	if len(events) != 1 || object(events[0])["id"] != eventID {
		t.Fatalf("same window must expose the original event %s: %+v", eventID, events)
	}
	isolated, err := pg.changeTimeline(ctx, "00000000-0000-4000-8000-000000000001", params)
	if err != nil || len(list(testRouteJSON(t, isolated)["changes"])) != 0 {
		t.Fatal("another asset's changes were exposed", err)
	}
}
