package store

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestWaitHistorySeparatesZeroFromMissing(t *testing.T) {
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if storeURL == "" {
		t.Skip("PostgreSQL 18 monitoring store is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pg, err := Open(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	groupName := " sample cluster "
	key := []byte("01234567890123456789012345678901")
	environment, tags := " production ", []string{" orders ", "orders"}
	input := assets.Input{Name: "wait history", ClusterName: &groupName, Environment: &environment, Tags: &tags, Host: "127.0.0.1", Port: 1,
		Database: "postgres", Username: "observer", Password: "test-secret", Runtime: "linux", SSLMode: "disable"}
	created, err := pg.CreateAsset(ctx, input, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID          string `json:"id"`
		ClusterName string `json:"clusterName"`
	}
	if err := json.Unmarshal(created, &asset); err != nil || asset.ClusterName != "sample cluster" {
		t.Fatalf("unexpected cluster configuration: %s, %v", created, err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	input.ClusterName = nil
	input.Environment, input.Tags = nil, nil
	updated, err := pg.UpdateAsset(ctx, asset.ID, input, key)
	if err != nil || json.Unmarshal(updated, &asset) != nil || asset.ClusterName != "sample cluster" {
		t.Fatalf("an old asset client lost the cluster: %s, %v", updated, err)
	}
	fleetQuery := FleetQuery{Page: 1, Limit: 10, Search: "sample cluster", Runtime: "linux", Environment: "production", Tag: "orders", Status: "coverage"}
	fleet, err := pg.MonitoringFleet(ctx, fleetQuery, []string{asset.ID})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Total    int `json:"total"`
		Clusters []struct {
			Members int `json:"members"`
			Pending int `json:"pending"`
		} `json:"clusters"`
	}
	if err := json.Unmarshal(fleet.(json.RawMessage), &page); err != nil || page.Total != 1 || len(page.Clusters) != 1 ||
		page.Clusters[0].Members != 1 || page.Clusters[0].Pending != 1 {
		t.Fatalf("unexpected scoped cluster summary: %s / %v", fleet, err)
	}
	empty, err := pg.MonitoringFleet(ctx, fleetQuery, []string{})
	if err != nil || json.Unmarshal(empty.(json.RawMessage), &page) != nil || page.Total != 0 {
		t.Fatalf("empty viewer scope exposed assets: %s / %v", empty, err)
	}
	base := time.Now().UTC().Truncate(time.Minute).Add(-5 * time.Minute)
	if err := pg.EnsureTimePartitions(ctx, base); err != nil {
		t.Fatal(err)
	}
	const queryID = "9223372036854775807"
	const active = `{"diagnostics":{"activeWaits":{"status":"ok","data":{"groups":[
		{"depth":3,"waitType":"Lock","sessions":2},
		{"depth":1,"waitType":"Lock","waitEvent":"advisory","sessions":2},
		{"depth":0,"waitType":"Lock","waitEvent":"advisory","queryId":"` + queryID + `","sessions":2}
	]}}}}`
	const idle = `{"diagnostics":{"activeWaits":{"status":"ok","data":{"groups":[]}}}}`
	for i, raw := range []string{active, idle, `{}`, idle} {
		offset := []time.Duration{5 * time.Second, 10 * time.Second, time.Minute, 2 * time.Minute}[i]
		if _, err := pg.pool.Exec(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
			VALUES(gen_random_uuid(),$1,$2,'ok',$3)`, asset.ID, base.Add(offset), raw); err != nil {
			t.Fatal(err)
		}
	}
	params := url.Values{"from": {base.Format(time.RFC3339Nano)}, "to": {base.Add(3 * time.Minute).Format(time.RFC3339Nano)},
		"bucketMinutes": {"1"}, "group": {"query"}, "waitType": {"Lock"}, "waitEvent": {"advisory"}}
	result, err := pg.ReadAssetSection(ctx, asset.ID, "wait-history", params)
	if err != nil {
		t.Fatal(err)
	}
	raw := result.(map[string]any)["buckets"].([]json.RawMessage)
	if len(raw) != 3 {
		t.Fatalf("want 3 observed time buckets, got %v", raw)
	}
	type bucket struct {
		Samples  int `json:"samples"`
		Observed int `json:"observed"`
		Groups   []struct {
			QueryID         string  `json:"queryId"`
			AverageSessions float64 `json:"averageSessions"`
		} `json:"groups"`
	}
	buckets := make([]bucket, len(raw))
	for i, value := range raw {
		if err := json.Unmarshal(value, &buckets[i]); err != nil {
			t.Fatal(err)
		}
	}
	if first := buckets[0]; first.Samples != 2 || first.Observed != 2 || len(first.Groups) != 1 ||
		first.Groups[0].AverageSessions != 1 || first.Groups[0].QueryID != queryID {
		t.Fatalf("wait average or stable query ID incorrect: %s", raw[0])
	}
	if buckets[1].Observed != 0 || buckets[2].Observed != 1 || len(buckets[2].Groups) != 0 {
		t.Fatalf("old samples and measured zero were confused: %s / %s", raw[1], raw[2])
	}
	if _, found, err := pg.ReadScopedCollection(ctx, "/api/monitoring/instances", []string{asset.ID}); err != nil || !found {
		t.Fatalf("scoped instance aggregation failed: %v", err)
	}
	if _, err := pg.ReadAssetSection(ctx, asset.ID, "monitoring-context", params); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ReadAssetSection(ctx, asset.ID, "sql", params); err != nil {
		t.Fatal(err)
	}
}
