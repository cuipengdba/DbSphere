package store

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestPG18HistoryTierPreservesEvidenceAndExactBuckets(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_HISTORY_PG18_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL 18 history fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	a, err := pg.CreateOrganization(ctx, "History evidence test")
	if err != nil {
		t.Fatal(err)
	}
	b, err := pg.CreateOrganization(ctx, "History other tenant")
	if err != nil {
		t.Fatal(err)
	}
	makeAsset := func(organization string) string {
		t.Helper()
		raw, err := pg.CreateAsset(ctx, assets.Input{OrganizationID: organization, Name: "History fixture", Host: "127.0.0.1",
			Port: 1, Database: "postgres", Username: "monitor", Password: "fixture-password", SSLMode: "disable", Runtime: "linux"},
			[]byte("01234567890123456789012345678901"))
		var result struct{ ID string }
		if err != nil || json.Unmarshal(raw, &result) != nil || result.ID == "" {
			t.Fatalf("cannot create history asset: %v", err)
		}
		return result.ID
	}
	asset, other := makeAsset(a.ID), makeAsset(b.ID)
	defer pg.DeleteAsset(context.Background(), asset)
	defer pg.DeleteAsset(context.Background(), other)
	now := time.Now().UTC().Truncate(time.Hour)
	old := now.Add(-49 * time.Hour)
	if err := pg.EnsureTimePartitions(ctx, old); err != nil {
		t.Fatal(err)
	}
	addSample := func(id string, at time.Time, connections int) string {
		t.Helper()
		var sample string
		err := pg.pool.QueryRow(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
			VALUES(gen_random_uuid(),$1,$2,'ok',jsonb_build_object('connections',$3::integer,'database_bytes',4096))
			RETURNING id::text`, id, at, connections).Scan(&sample)
		if err != nil {
			t.Fatal(err)
		}
		return sample
	}
	archived := addSample(asset, old.Add(5*time.Minute), 10)
	addSample(asset, old.Add(10*time.Minute), 20)
	pinned := addSample(asset, old.Add(15*time.Minute), 40)
	foreign := addSample(other, old.Add(15*time.Minute), 61)
	addSample(asset, now.Add(-40*time.Minute), 12)
	var event string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
		VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, asset, old).Scan(&event); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,'collection_failure','History evidence test','open',$3,'availability')`, event, asset, old); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
		VALUES(gen_random_uuid(),$1,'collection_error',$2,jsonb_build_object('sampleId',$3::text))`, event, old, pinned); err != nil {
		t.Fatal(err)
	}
	tenant := WithOrganization(ctx, a.ID)
	if _, err := pg.SetHistoryTierPolicy(tenant, HistoryTierPolicy{OrganizationID: b.ID, Metric: "connections", RawDays: 1, HourlyDays: 90}); !errors.Is(err, ErrHistoryTierInput) {
		t.Fatalf("cross-organization policy was accepted: %v", err)
	}
	if _, err := pg.SetHistoryTierPolicy(tenant, HistoryTierPolicy{Metric: "connections", RawDays: 1, HourlyDays: 90}); err != nil {
		t.Fatal(err)
	}
	resetFixtureCursor := func() {
		t.Helper()
		if _, err := pg.pool.Exec(ctx, `UPDATE history_tier_policy SET archive_through=$2
			WHERE organization_id=$1 AND metric='connections'`, a.ID, old); err != nil {
			t.Fatal(err)
		}
	}
	resetFixtureCursor()
	if _, err := pg.pool.Exec(ctx, `INSERT INTO history_tier_hours(asset_id,metric,bucket,samples,total,low,peak)
		VALUES($1,'connections',$2,1,100,100,100)`, asset, now.AddDate(0, 0, -91)); err != nil {
		t.Fatal(err)
	}
	if result, err := pg.AdvanceHistoryTier(ctx, now, 5000); err != nil || result.Batched != 2 || result.RemovedHours != 1 {
		t.Fatalf("expected two compacted fields: %+v %v", result, err)
	}
	resetFixtureCursor()
	if result, err := pg.AdvanceHistoryTier(ctx, now, 5000); err != nil || result.Batched != 0 {
		t.Fatalf("retry counted a field twice: %+v %v", result, err)
	}
	for _, row := range []struct {
		id       string
		expected int
	}{{pinned, 40}, {foreign, 61}, {archived, 0}} {
		var connections, bytes int
		err := pg.pool.QueryRow(ctx, `SELECT coalesce((metrics_json->>'connections')::int,0),
			(metrics_json->>'database_bytes')::int FROM samples WHERE id=$1`, row.id).Scan(&connections, &bytes)
		if err != nil || connections != row.expected || bytes != 4096 {
			t.Fatalf("raw metric scope or pinned sample mismatch: %s %d %d %v", row.id, connections, bytes, err)
		}
	}
	params := url.Values{"from": {old.Format(time.RFC3339)}, "to": {now.Format(time.RFC3339)}}
	trend, err := pg.ReadHistoryTier(ctx, asset, "connections", params)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(trend)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Buckets []struct {
			Samples           int
			Average, Min, Max float64
			Hourly, Raw       bool
		}
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Buckets) != 2 ||
		result.Buckets[0].Samples != 3 || result.Buckets[0].Average != 23.333 || result.Buckets[0].Min != 10 ||
		result.Buckets[0].Max != 40 || !result.Buckets[0].Hourly || !result.Buckets[0].Raw ||
		result.Buckets[1].Average != 12 || result.Buckets[1].Hourly {
		t.Fatalf("mixed-tier result changed units, totals or sample coverage: %s", raw)
	}
	if _, err := pg.PruneSamples(ctx, now.Add(-24*time.Hour), 1000); err != nil {
		t.Fatal(err)
	}
	var evidenceIntact bool
	if err := pg.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM samples WHERE id=$1 AND metrics_json->>'connections'='40')`,
		pinned).Scan(&evidenceIntact); err != nil || !evidenceIntact {
		t.Fatalf("global retention removed original cited evidence: %v", err)
	}
	t.Logf("asset=%s event=%s pinned=%s average=23.333 samples=3 cold=2 retainedRaw=1", asset, event, pinned)
}
