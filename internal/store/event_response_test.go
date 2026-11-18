package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
)

func TestEventResponseAndRecoveryProofInPG18(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	ctx := context.Background()
	pg, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Response fixture", Host: "127.0.0.1", Port: 1,
		Database: "postgres", Username: "monitor", Password: "fixture-password", SSLMode: "disable", Runtime: "linux"},
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
	start := time.Now().UTC().Add(-2 * time.Minute)
	var eventID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO event_keys(id,asset_id,started_at)
		VALUES(gen_random_uuid(),$1,$2) RETURNING id::text`, asset.ID, start).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	defer pg.pool.Exec(ctx, `DELETE FROM event_keys WHERE id=$1`, eventID)
	if _, err := pg.pool.Exec(ctx, `INSERT INTO events(id,asset_id,kind,title,status,started_at,event_group)
		VALUES($1,$2,'latency','Response fixture','open',$3,'latency')`, eventID, asset.ID, start); err != nil {
		t.Fatal(err)
	}
	if err := pg.RecordEventResponse(ctx, eventID, "diagnosis", "bad", ""); !errors.Is(err, ErrInvalidEventResponse) {
		t.Fatalf("short note accepted: %v", err)
	}
	if err := pg.RecordEventResponse(ctx, eventID, "mitigation", "已调整应用侧连接池，待恢复采样验证", ""); err != nil {
		t.Fatal(err)
	}
	var failureID, successID string
	if err := pg.pool.QueryRow(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
		VALUES(gen_random_uuid(),$1,now(),'error','{}') RETURNING id::text`, asset.ID).Scan(&failureID); err != nil {
		t.Fatal(err)
	}
	if err := pg.RecordEventResponse(ctx, eventID, "recovery_verification", "检查连接和锁等待已恢复", failureID); !errors.Is(err, ErrRecoveryNotVerified) {
		t.Fatalf("open event was verified: %v", err)
	}
	ended := time.Now().UTC().Add(-time.Minute)
	if _, err := pg.pool.Exec(ctx, `UPDATE events SET status='resolved',ended_at=$2 WHERE id=$1`, eventID, ended); err != nil {
		t.Fatal(err)
	}
	if err := pg.RecordEventResponse(ctx, eventID, "recovery_verification", "检查连接和锁等待已恢复", failureID); !errors.Is(err, ErrRecoveryNotVerified) {
		t.Fatalf("failure sample verified recovery: %v", err)
	}
	if err := pg.pool.QueryRow(ctx, `INSERT INTO samples(id,asset_id,collected_at,status,metrics_json)
		VALUES(gen_random_uuid(),$1,now(),'ok','{}') RETURNING id::text`, asset.ID).Scan(&successID); err != nil {
		t.Fatal(err)
	}
	const note = "恢复采样确认连接和锁等待正常，人工备注 private_action"
	if err := pg.RecordEventResponse(ctx, eventID, "recovery_verification", note, successID); err != nil {
		t.Fatal(err)
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	if !strings.Contains(string(encoded), "recovery_verified") || !strings.Contains(string(encoded), successID) {
		t.Fatal("recovery evidence was not attached to same event")
	}
	report, err := pg.EventReport(ctx, eventID, "md")
	if err != nil || !strings.Contains(report, note) || !strings.Contains(report, successID) {
		t.Fatal("event report lost response and recovery proof", err)
	}
	aiPrompt, _, err := pg.EventAIEvidence(ctx, eventID)
	if err != nil || strings.Contains(aiPrompt, "private_action") || !strings.Contains(aiPrompt, "[人工备注已省略]") {
		t.Fatal("AI evidence exposed operator note", err)
	}
}
