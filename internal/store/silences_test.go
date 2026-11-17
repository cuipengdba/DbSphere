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

func TestSilenceWindowValidation(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	valid := SilenceInput{EndsAt: now.Add(time.Hour).Format(time.RFC3339), Reason: " planned maintenance "}
	got, err := valid.normalize(now)
	if err != nil || got.Kind != "silence" || got.Reason != "planned maintenance" || got.StartsAt != now.Format(time.RFC3339) {
		t.Fatalf("valid silence window rejected: %+v %v", got, err)
	}
	for _, change := range []func(*SilenceInput){
		func(v *SilenceInput) { v.Kind = "other" },
		func(v *SilenceInput) { v.AssetID = "not-a-uuid" },
		func(v *SilenceInput) { v.RuleID = "not a rule" },
		func(v *SilenceInput) { v.Reason = "  " },
		func(v *SilenceInput) { v.EndsAt = now.Add(31 * 24 * time.Hour).Format(time.RFC3339) },
		func(v *SilenceInput) { v.StartsAt = now.Add(-6 * time.Minute).Format(time.RFC3339) },
	} {
		input := valid
		change(&input)
		if _, err := input.normalize(now); !errors.Is(err, ErrInvalidSilence) {
			t.Fatalf("invalid silence accepted: %+v, %v", input, err)
		}
	}
}

func TestGoSilenceCreateAndRevokePG18(t *testing.T) {
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
	if _, err := pg.CreateSilence(ctx, SilenceInput{RuleID: "missing_rule", Reason: "test",
		EndsAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)}); !errors.Is(err, ErrSilenceReference) {
		t.Fatalf("unknown rule accepted: %v", err)
	}
	created, err := pg.CreateSilence(ctx, SilenceInput{RuleID: "pg_connections_high", Kind: "maintenance",
		Reason: "PG maintenance test", EndsAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	var silence struct {
		ID      string     `json:"id"`
		Kind    string     `json:"kind"`
		RuleID  string     `json:"ruleId"`
		Revoked *time.Time `json:"revokedAt"`
	}
	if err := json.Unmarshal(created, &silence); err != nil || silence.ID == "" ||
		silence.Kind != "maintenance" || silence.RuleID != "pg_connections_high" {
		t.Fatalf("created silence mismatch: %s %v", created, err)
	}
	defer pg.pool.Exec(ctx, `DELETE FROM silences WHERE id=$1`, silence.ID)
	revoked, err := pg.RevokeSilence(ctx, silence.ID)
	if err != nil || json.Unmarshal(revoked, &silence) != nil || silence.Revoked == nil {
		t.Fatalf("silence not revoked: %s %v", revoked, err)
	}
	if _, err := pg.RevokeSilence(ctx, silence.ID); !errors.Is(err, ErrSilenceNotFound) {
		t.Fatalf("silence revoked twice: %v", err)
	}
}

func TestGoSilenceSuppressionSharesIncidentEvidence(t *testing.T) {
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
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Go silence incident test",
		Host: "127.0.0.1", Port: 5432, Database: "postgres", Username: "postgres",
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
	now := time.Now().UTC()
	window, err := pg.CreateSilence(ctx, SilenceInput{AssetID: asset.ID, RuleID: "pg_lock_wait",
		Reason: "planned lock test", EndsAt: now.Add(time.Hour).Format(time.RFC3339Nano)})
	if err != nil {
		t.Fatal(err)
	}
	var silence struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(window, &silence); err != nil {
		t.Fatal(err)
	}
	metrics := map[string]any{"version_num": 180000, "connections": 1, "max_connections": 100,
		"diagnostics": map[string]any{"locks": map[string]any{"status": "ok",
			"data": map[string]any{"blockedSessions": 1}}}}
	for _, at := range []time.Time{now, now.Add(61 * time.Second)} {
		if _, err := pg.recordCheckAt(ctx, asset.ID, metrics, "", "", at); err != nil {
			t.Fatal(err)
		}
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1
		AND event_group='latency' AND status='open'`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.RevokeSilence(ctx, silence.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.recordCheckAt(ctx, asset.ID, metrics, "", "", now.Add(62*time.Second)); err != nil {
		t.Fatal(err)
	}
	var suppressed, unsuppressed int
	if err := pg.pool.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE kind='alert_suppressed')::int,
		count(*) FILTER (WHERE kind='alert_unsuppressed')::int
		FROM evidence WHERE event_id=$1`, eventID).Scan(&suppressed, &unsuppressed); err != nil ||
		suppressed != 1 || unsuppressed != 1 {
		t.Fatalf("same incident did not record suppression transitions: %d %d %v", suppressed, unsuppressed, err)
	}
}
