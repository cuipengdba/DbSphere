package store

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestMonitoringOverviewScopesTimelineAndCurrentAlerts(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if storeURL == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	ctx := context.Background()
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	create := func(name string) string {
		t.Helper()
		var id string
		err := pg.pool.QueryRow(ctx, `INSERT INTO assets(id,name,host,port,database_name,username,password_enc,ssl_mode,runtime,created_at)
			VALUES(gen_random_uuid(),$1,'127.0.0.1',5432,'postgres','monitor','','disable','linux',now()) RETURNING id`, name).Scan(&id)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pg.pool.Exec(ctx, "DELETE FROM assets WHERE id=$1", id) })
		return id
	}
	visible, hidden := create("Overview visible"), create("Overview hidden")
	to := time.Now().UTC().Truncate(time.Minute)
	from := to.Add(-36 * time.Minute)
	insert := func(asset string, start time.Time, finish *time.Time) {
		t.Helper()
		_, err := pg.pool.Exec(ctx, `WITH k AS (
			INSERT INTO event_keys(id,asset_id,started_at) VALUES(gen_random_uuid(),$1,$2) RETURNING id
		), e AS (
			INSERT INTO events(id,asset_id,kind,title,status,started_at,ended_at,event_group)
			SELECT id,$1,'availability','Test event',CASE WHEN $3::timestamptz IS NULL THEN 'open' ELSE 'resolved' END,
				$2,$3,'availability' FROM k RETURNING id
		) INSERT INTO open_event_groups(asset_id,event_group,event_id)
		SELECT $1,'availability',id FROM e WHERE $3::timestamptz IS NULL`, asset, start, finish)
		if err != nil {
			t.Fatal(err)
		}
	}
	ended := from.Add(time.Minute)
	insert(visible, from.Add(-time.Minute), &ended)
	insert(visible, from.Add(2*time.Minute), nil)
	insert(hidden, from.Add(3*time.Minute), nil)
	if _, err := pg.pool.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		SELECT gen_random_uuid(),event_id,'alert_firing',$2,'{"severity":"critical"}' FROM open_event_groups WHERE asset_id=$1`, visible, from.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO asset_changes(asset_id,kind,label,source,occurred_at)
		VALUES($1,'maintenance','Test maintenance','operator',$2)`, visible, from.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	selectionEnd := from.Add(4 * time.Minute)
	q := OverviewQuery{From: from, To: to, InspectFrom: &ended, InspectTo: &selectionEnd}
	read := func(ids []string) (result struct {
		Summary        struct{ Assets, Unavailable, Events int }
		Assets, Alerts []json.RawMessage
		Timeline       []struct{ Opened, Recovered, Availability, Maintenance int }
		Inspection     struct {
			Rows []struct{ ID, AssetID, Phase, Severity string }
		}
	}) {
		t.Helper()
		raw, err := pg.MonitoringOverview(ctx, ids, q)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	result := read([]string{visible})
	if result.Summary.Assets != 1 || result.Summary.Unavailable != 1 || result.Summary.Events != 1 ||
		len(result.Assets) != 1 || len(result.Alerts) != 1 || len(result.Timeline) != 36 {
		t.Fatalf("scope or timeline size mismatch: %+v", result.Summary)
	}
	bins := result.Timeline
	if bins[0].Availability != 1 || bins[1].Recovered != 1 || bins[1].Availability != 0 ||
		bins[2].Opened != 1 || bins[2].Availability != 1 || bins[3].Opened != 0 || bins[3].Availability != 1 || bins[3].Maintenance != 1 {
		t.Fatalf("unexpected bin boundaries: %+v", bins[:4])
	}
	rows := result.Inspection.Rows
	if len(rows) != 3 || rows[0].Phase != "maintenance" || rows[1].Phase != "opened" || rows[1].Severity != "critical" ||
		rows[2].Phase != "recovered" || rows[0].AssetID != visible || rows[1].AssetID != visible || rows[2].AssetID != visible {
		t.Fatalf("invalid event inspection: %+v", rows)
	}
	empty := read([]string{})
	if empty.Summary.Assets != 0 || len(empty.Alerts) != 0 || empty.Timeline[2].Opened != 0 || len(empty.Inspection.Rows) != 0 {
		t.Fatal("empty authorization scope included assets or events")
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE assets SET environment=CASE WHEN id=$1 THEN 'production' ELSE 'staging' END
		WHERE id IN ($1,$2)`, visible, hidden); err != nil {
		t.Fatal(err)
	}
	raw, err := pg.LiveObservation(ctx, []string{visible, hidden}, 60, LiveScope{Environment: "production", Engine: "postgres"})
	if err != nil {
		t.Fatal(err)
	}
	var live struct {
		Total, Unavailable int
		Assets             []struct{ ID string }
		Events             []struct{ AssetID, Severity string }
	}
	if err := json.Unmarshal(raw, &live); err != nil {
		t.Fatal(err)
	}
	if live.Total != 1 || live.Unavailable != 1 || len(live.Assets) != 1 || live.Assets[0].ID != visible ||
		len(live.Events) == 0 || live.Events[0].AssetID != visible || live.Events[0].Severity != "critical" {
		t.Fatalf("live scope and severity mismatch: %+v", live)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE assets SET environment='production', linux_host='wallboard-fixture', linux_exporter_port=9100
		WHERE id IN ($1,$2)`, visible, hidden); err != nil {
		t.Fatal(err)
	}
	raw, err = pg.LiveObservation(ctx, []string{visible, hidden}, 60, LiveScope{Wallboard: true, Focus: visible, Environment: "production"})
	if err != nil {
		t.Fatal(err)
	}
	var board struct {
		Total, HostCount, MissingCount int
		Assets                         []struct {
			ID       string
			WallHost struct{ ID, Name string }
		}
	}
	if err := json.Unmarshal(raw, &board); err != nil {
		t.Fatal(err)
	}
	if board.Total != 2 || board.HostCount != 1 || board.MissingCount != 2 || len(board.Assets) != 2 ||
		board.Assets[0].WallHost.ID == "" {
		t.Fatalf("incorrect wallboard summary: %s", raw)
	}
	if board.Assets[0].WallHost.ID != board.Assets[1].WallHost.ID || board.Assets[0].ID != visible {
		t.Fatalf("wallboard lost shared host identity or focused asset: %s", raw)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
		SELECT gen_random_uuid(),$1,$2::timestamptz+step*interval '60 seconds','ok',
		jsonb_build_object('connections',5,'max_connections',10,'xact_commit',20+120*step,'xact_rollback',0,
		'role','primary','databaseName','postgres','postgresStartedAt','2020-01-01T00:00:00Z',
		'postgresEndpoint',jsonb_build_object('address','127.0.0.1','port',5432))
		FROM generate_series(0,1) step`, visible, from); err != nil {
		t.Fatal(err)
	}
	params := url.Values{"from": {from.Add(30 * time.Second).Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}}
	value, err := pg.ReadAssetSection(ctx, visible, "wallboard-history", params)
	if err != nil {
		t.Fatal(err)
	}
	history := value.(map[string]any)
	points := history["points"].([]wallboardPoint)
	if len(points) != 1 || points[0].SampleID == "" || points[0].Values["connections"] != 50 || points[0].Values["tps"] != 2 ||
		len(history["events"].([]json.RawMessage)) != 2 || len(history["changes"].([]json.RawMessage)) != 1 {
		t.Fatalf("wallboard did not preserve sample, pre-range counters and timeline: %+v", history)
	}
}
