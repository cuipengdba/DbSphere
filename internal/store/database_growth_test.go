package store

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestDatabaseGrowthSignalRequiresObservedHistory(t *testing.T) {
	metric := "pg.database_growth_bytes_per_day"
	if readGoSignal(metric, map[string]any{"database_bytes": float64(12 << 30)}, time.Now()).Available {
		t.Fatal("database size alone was treated as an observed trend")
	}
	metrics := map[string]any{"databaseGrowth": map[string]any{"status": "ok",
		"growthBytesPerDay": float64(12 << 30), "baselineSampleId": "sample-1"}}
	signal := readGoSignal(metric, metrics, time.Now())
	if !signal.Available || signal.Value != float64(12<<30) ||
		object(signal.Detail)["baselineSampleId"] != "sample-1" {
		t.Fatalf("observed database growth lost its source: %+v", signal)
	}
	if !supportedAlertMetric(metric) {
		t.Fatal("database growth rule is not supported")
	}
}

func TestDatabaseGrowthRuleGroupUpgradePreservesOperatorSettings(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	ctx := context.Background()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	tx, err := pg.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE alert_rules SET definition_json=
		jsonb_set(jsonb_set(jsonb_set(definition_json,'{group}','"capacity"'::jsonb),
		'{threshold}','123'::jsonb),'{enabled}','false'::jsonb)
		WHERE id='pg_database_growth_fast'`); err != nil {
		t.Fatal(err)
	}
	if err := seedGoRules(ctx, tx); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT definition_json FROM alert_rules
		WHERE id='pg_database_growth_fast'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var rule alertRule
	if err := json.Unmarshal(raw, &rule); err != nil {
		t.Fatal(err)
	}
	if rule.Group != "storage" || rule.Threshold != 123 || rule.Enabled {
		t.Fatalf("preview rule migration lost operator choices: %+v", rule)
	}
}

func TestDatabaseGrowthAlertUsesPG18BaselineAndSameEventRecovery(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	ctx := context.Background()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Database growth rule test",
		Host: "127.0.0.1", Port: 1, Database: "postgres", Username: "monitor",
		Password: "test-password", SSLMode: "disable", Runtime: "linux"},
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
	metrics := func(bytes int64, diskPercent float64) map[string]any {
		return map[string]any{"database_bytes": bytes,
			"collector": map[string]any{"pgStatus": "ok"},
			"linux": map[string]any{"status": "ok", "filesystems": []any{
				map[string]any{"mountpoint": "/var/lib/postgresql", "usedPercent": diskPercent}}}}
	}
	anchor := time.Now().UTC().Truncate(time.Second).Add(-10 * time.Minute)
	const gib = int64(1024 * 1024 * 1024)
	for _, item := range []struct {
		at    time.Time
		bytes int64
	}{
		{anchor.Add(-24 * time.Hour), gib},
		{anchor.Add(130*time.Second - 24*time.Hour), gib},
		{anchor.Add(310*time.Second - 24*time.Hour), gib},
		{anchor.Add(600*time.Second - 24*time.Hour), 13 * gib},
	} {
		if _, err := pg.recordCheckAt(ctx, asset.ID, metrics(item.bytes, 40), "", "", item.at); err != nil {
			t.Fatal(err)
		}
	}
	first, err := pg.recordCheckAt(ctx, asset.ID, metrics(13*gib, 95), "", "", anchor)
	if err != nil {
		t.Fatal(err)
	}
	var sample []byte
	if err := pg.pool.QueryRow(ctx, `SELECT metrics_json FROM samples WHERE asset_id=$1 AND id=$2`,
		asset.ID, first.SampleID).Scan(&sample); err != nil {
		t.Fatal(err)
	}
	var observed map[string]any
	if err := json.Unmarshal(sample, &observed); err != nil {
		t.Fatal(err)
	}
	if signal := readGoSignal("pg.database_growth_bytes_per_day", observed, anchor); !signal.Available || signal.Value < 10*float64(gib) {
		t.Fatalf("24-hour growth not recorded on sample: %+v", signal)
	}
	var firing int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*)::int FROM alert_states WHERE asset_id=$1
		AND rule_id='pg_database_growth_fast' AND state_json->>'status'='firing'`,
		asset.ID).Scan(&firing); err != nil || firing != 0 {
		t.Fatalf("trend rule fired before sustained growth: %d %v", firing, err)
	}
	second, err := pg.recordCheckAt(ctx, asset.ID, metrics(13*gib, 95), "", "", anchor.Add(130*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1
		AND event_group='storage' AND status='open'`, asset.ID).Scan(&eventID); err != nil {
		t.Fatalf("sustained growth did not open a storage incident: %v", err)
	}
	var firingPayload []byte
	if err := pg.pool.QueryRow(ctx, `SELECT payload_json FROM evidence WHERE event_id=$1
		AND kind='alert_firing' ORDER BY observed_at DESC LIMIT 1`, eventID).Scan(&firingPayload); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(firingPayload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["metric"] != "pg.database_growth_bytes_per_day" ||
		payload["sampleId"] != second.SampleID ||
		object(payload["detail"])["baselineSampleId"] == "" {
		t.Fatalf("capacity incident lacks baseline evidence: %+v", payload)
	}
	if _, err := pg.recordCheckAt(ctx, asset.ID, metrics(13*gib, 95), "", "", anchor.Add(310*time.Second)); err != nil {
		t.Fatal(err)
	}
	var sameEventRules int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*)::int FROM alert_states WHERE asset_id=$1
		AND state_json->>'eventId'=$2 AND state_json->>'status'='firing'
		AND rule_id IN ('pg_database_growth_fast','linux_disk_high')`, asset.ID, eventID).
		Scan(&sameEventRules); err != nil || sameEventRules != 2 {
		t.Fatalf("growth and Linux disk pressure did not share one event: %d %v", sameEventRules, err)
	}
	result, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	baselineID := stringValue(object(payload["detail"])["baselineSampleId"])
	graph := object(object(result.(map[string]any)["analysis"])["graph"])
	baselineLinked := false
	for _, edge := range graph["edges"].([]map[string]string) {
		if edge["to"] == "sample:"+baselineID && edge["relation"] == "compared_with" {
			baselineLinked = true
		}
	}
	if !baselineLinked {
		t.Fatal("growth baseline sample has no raw-data link in the event graph")
	}
	correlated := false
	for _, candidate := range object(result.(map[string]any)["analysis"])["candidates"].([]map[string]any) {
		if candidate["code"] == "storage_growth_pressure" && candidate["confidence"] == "low" {
			correlated = true
			foundBaseline := false
			for _, support := range candidate["supports"].([]string) {
				foundBaseline = foundBaseline || support == "sample:"+baselineID
			}
			if !foundBaseline {
				t.Fatal("storage root-cause candidate lacks baseline sample support")
			}
		}
	}
	if !correlated {
		t.Fatal("same-event growth and disk pressure lacked a cautious RCA candidate")
	}
	if _, err := pg.recordCheckAt(ctx, asset.ID, metrics(13*gib, 40), "", "", anchor.Add(600*time.Second)); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pg.pool.QueryRow(ctx, `SELECT status FROM events WHERE id=$1`, eventID).Scan(&status); err != nil || status != "resolved" {
		t.Fatalf("growth recovery split or left incident open: %s %v", status, err)
	}
}
