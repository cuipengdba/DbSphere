package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestAlertReplayHoldsRecoveryAndHonorsSilence(t *testing.T) {
	start, err := time.Parse(time.RFC3339, "2026-11-01T01:58:00-04:00")
	if err != nil {
		t.Fatal(err)
	}
	reading := func(id string, seconds int, value float64, available, muted bool) alertReplaySignal {
		return alertReplaySignal{ID: id, At: start.Add(time.Duration(seconds) * time.Second).UTC(),
			Value: alertSignal{Available: available, Value: value}, Muted: muted}
	}
	signals := []alertReplaySignal{
		reading("spike", 0, 1, true, false), reading("drop", 4, 0, true, false),
		reading("breach", 20, 2, true, true), reading("short", 25, 2, true, true),
		reading("sustained", 30, 2, true, true), reading("unmute", 34, 2, true, false),
		reading("soft_recovery", 40, 0, true, false), reading("gap", 42, 0, false, false),
		reading("relapse", 45, 1, true, false), reading("new_recovery", 60, 0, true, false),
		reading("complete", 65, 0, true, false),
	}
	base := alertRule{Enabled: true, Operator: ">=", Threshold: 1, DurationSeconds: 10, RecoverySeconds: 5}
	candidate := base
	candidate.DurationSeconds = 5
	rows := compareAlertReplay(replayAlertEpisodes(base, signals), replayAlertEpisodes(candidate, signals))
	if len(rows) != 1 || rows[0].State != "earlier" || rows[0].ShiftSecs != -5 {
		t.Fatalf("unexpected comparison: %+v", rows)
	}
	got := rows[0]
	if !got.Proposed.Suppressed || got.Proposed.NotifyAt == nil || !got.Proposed.NotifyAt.Equal(signals[5].At) ||
		got.Current.RecoverySample != "complete" || got.Proposed.RecoverySample != "complete" ||
		got.Proposed.RecoverySeconds == nil || *got.Proposed.RecoverySeconds != 40 {
		t.Fatalf("maintenance, missing samples or relapse were lost: %+v / %+v", got.Current, got.Proposed)
	}
}

func TestAlertRevisionPG18ShadowAndScopedEnable(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	ctx := context.Background()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pg.Close)
	key := []byte("01234567890123456789012345678901")
	makeAsset := func(name string) string {
		t.Helper()
		raw, err := pg.CreateAsset(ctx, assets.Input{Name: name, Runtime: "linux", Host: "127.0.0.1", Port: 5432,
			Database: "postgres", Username: "postgres", Password: "test-fixture", SSLMode: "disable"}, key)
		var asset struct{ ID string }
		if err != nil || json.Unmarshal(raw, &asset) != nil || asset.ID == "" {
			t.Fatalf("create asset: %v", err)
		}
		t.Cleanup(func() { _, _ = pg.DeleteAsset(ctx, asset.ID) })
		return asset.ID
	}
	asset := makeAsset("Rule replay canary")
	sibling := makeAsset("Rule replay sibling")
	base := time.Now().UTC().Truncate(time.Second).Add(-2 * time.Minute)
	check := func(asset string, now time.Time, blocked int) string {
		t.Helper()
		result, err := pg.recordCheckAt(ctx, asset, map[string]any{"version_num": 180007,
			"diagnostics": map[string]any{"locks": map[string]any{"status": "ok", "data": map[string]any{"blockedSessions": blocked}}},
			"collector": map[string]any{"pgStatus": "ok"}}, "", "", now)
		if err != nil {
			t.Fatal(err)
		}
		return result.SampleID
	}
	check(asset, base, 1)
	check(asset, base.Add(5*time.Second), 0)
	zero := 0
	const rule = "pg_lock_wait"
	input := AlertReplayInput{From: base, To: base.Add(time.Minute), Patch: AlertRulePatch{DurationSeconds: &zero}}
	if _, err := pg.ReplayAlertRule(WithOrganization(ctx, "eeeeeeee-1234-4123-abcd-112233445566"), asset, rule, input, false); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("cross-organization replay was not rejected: %v", err)
	}
	value, err := pg.ReplayAlertRule(ctx, asset, rule, input, true)
	if err != nil {
		t.Fatal(err)
	}
	result := value.(map[string]any)
	revision := result["revisionId"].(string)
	replay := result["replay"].(alertReplayResult)
	if len(replay.Comparisons) != 1 || replay.Comparisons[0].State != "added" {
		t.Fatalf("historical lock spike comparison: %+v", replay)
	}
	if _, err := pg.ChangeAlertRevision(ctx, asset, rule, revision, AlertRevisionAction{Action: "enable", Scope: "asset"}); !errors.Is(err, ErrAlertRevisionConflict) {
		t.Fatalf("revision enabled before observation and review: %v", err)
	}
	current := time.Now().UTC().Truncate(time.Second)
	shadow := check(asset, current, 1)
	var sample, state string
	if err := pg.pool.QueryRow(ctx, `SELECT shadow_sample::text,shadow_transition FROM alert_rule_revisions WHERE id=$1`, revision).
		Scan(&sample, &state); err != nil || sample != shadow || state != "firing" {
		t.Fatalf("shadow observation missing: %s %s %v", sample, state, err)
	}
	if _, err := pg.ChangeAlertRevision(WithAccessActor(ctx, "reviewer"), asset, rule, revision, AlertRevisionAction{Action: "review", Note: "核对锁等待"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.ChangeAlertRevision(ctx, asset, rule, revision, AlertRevisionAction{Action: "enable", Scope: "asset"}); err != nil {
		t.Fatal(err)
	}
	check(sibling, current, 1)
	fault := check(asset, current.Add(time.Second), 1)
	var event string
	if err := pg.pool.QueryRow(ctx, `SELECT e.id::text FROM events e JOIN evidence x ON x.event_id=e.id
		WHERE e.asset_id=$1 AND x.kind='alert_firing' AND x.payload_json->>'ruleRevisionId'=$2 AND x.payload_json->>'sampleId'=$3`,
		asset, revision, fault).Scan(&event); err != nil {
		t.Fatalf("canary firing did not persist the original revision: %v", err)
	}
	var count int
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE asset_id=$1 AND event_group='latency'`, sibling).Scan(&count); err != nil || count != 0 {
		t.Fatalf("canary changed sibling rules: %d, %v", count, err)
	}
	if _, err := pg.ChangeAlertRevision(ctx, asset, rule, revision, AlertRevisionAction{Action: "revert"}); err != nil {
		t.Fatal(err)
	}
	recovery := check(asset, current.Add(2*time.Second), 0)
	if err := pg.pool.QueryRow(ctx, `SELECT count(*) FROM events e JOIN evidence x ON x.event_id=e.id
		WHERE e.id=$1 AND e.status='resolved' AND x.kind='alert_recovered' AND x.payload_json->>'ruleRevisionId'=$2
		AND x.payload_json->>'sampleId'=$3`, event, revision, recovery).Scan(&count); err != nil || count != 1 {
		t.Fatalf("original incident did not keep revision through recovery: %d, %v", count, err)
	}
}
