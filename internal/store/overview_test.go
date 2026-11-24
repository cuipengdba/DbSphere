package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

func TestMonitoringOverviewScopesTimelineAndCurrentAlerts(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	ctx := context.Background()
	pg, err := Open(ctx, url)
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
	q := OverviewQuery{From: from, To: to}
	read := func(ids []string) (result struct {
		Summary        struct{ Assets, Unavailable, Events int }
		Assets, Alerts []json.RawMessage
		Timeline       []struct{ Opened, Recovered, Availability int }
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
		bins[2].Opened != 1 || bins[2].Availability != 1 || bins[3].Opened != 0 || bins[3].Availability != 1 {
		t.Fatalf("unexpected bin boundaries: %+v", bins[:4])
	}
	empty := read([]string{})
	if empty.Summary.Assets != 0 || len(empty.Alerts) != 0 || empty.Timeline[2].Opened != 0 {
		t.Fatal("empty authorization scope included assets or events")
	}
}
