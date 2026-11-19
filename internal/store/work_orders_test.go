package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/security"
)

func TestPG18WorkOrderRequiresSeparateDecisionAndSingleExecution(t *testing.T) {
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
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "work order isolation", Host: "127.0.0.1",
		Port: 1, Database: "postgres", Username: "postgres", Password: "test-secret",
		SSLMode: "disable", Runtime: "linux"}, key)
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
	if _, err := pg.CheckAsset(ctx, asset.ID, key); err != nil {
		t.Fatal(err)
	}
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1 AND status='open'`,
		asset.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	order, err := pg.CreateWorkOrder(ctx, eventID, "复查当前数据库连接故障")
	if err != nil || !strings.Contains(string(order), `"status": "requested"`) &&
		!strings.Contains(string(order), `"status":"requested"`) {
		t.Fatalf("request: %s %v", order, err)
	}
	var data struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(order, &data); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pg.BeginWorkOrder(ctx, data.ID); !errors.Is(err, ErrWorkOrderConflict) {
		t.Fatal("collection started without separate approval", err)
	}
	if _, err := pg.DecideWorkOrder(ctx, data.ID, "approved"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.DecideWorkOrder(ctx, data.ID, "rejected"); !errors.Is(err, ErrWorkOrderConflict) {
		t.Fatal("approval was overwritten", err)
	}
	_, targetID, err := pg.BeginWorkOrder(ctx, data.ID)
	if err != nil || targetID != asset.ID {
		t.Fatal("approved work order did not bind to its asset", err)
	}
	if _, _, err := pg.BeginWorkOrder(ctx, data.ID); !errors.Is(err, ErrWorkOrderConflict) {
		t.Fatal("work order executed twice", err)
	}
	result, collectionErr := pg.CheckAsset(ctx, targetID, key)
	finished, err := pg.FinishWorkOrder(ctx, data.ID, result, collectionErr)
	if err != nil || !strings.Contains(string(finished), result.SampleID) {
		t.Fatal("result was not recorded", err)
	}
	if _, err := pg.FinishWorkOrder(ctx, data.ID, result, collectionErr); !errors.Is(err, ErrWorkOrderConflict) {
		t.Fatal("execution receipt was overwritten", err)
	}
	report, err := pg.EventReport(ctx, eventID, "md")
	if err != nil || !strings.Contains(report, "work_order_requested") ||
		!strings.Contains(report, "work_order_decision") || !strings.Contains(report, "work_order_executed") {
		t.Fatalf("same-event work order evidence missing: %v", err)
	}
	aiPacket, allowed, err := pg.EventAIEvidence(ctx, eventID)
	if err != nil || strings.Contains(aiPacket, data.ID) {
		t.Fatalf("internal approval data entered AI packet: %v", err)
	}
	var decisionEvidenceID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM evidence WHERE event_id=$1
		AND kind='work_order_decision' ORDER BY observed_at DESC LIMIT 1`, eventID).Scan(&decisionEvidenceID); err != nil {
		t.Fatal(err)
	}
	for _, id := range allowed {
		if id == decisionEvidenceID {
			t.Fatal("approval became root cause evidence")
		}
	}
	second, err := pg.CreateWorkOrder(ctx, eventID, "再次复查当前数据库故障")
	if err != nil {
		t.Fatal(err)
	}
	var pending struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(second, &pending); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE work_orders SET expires_at=now()-interval '1 minute' WHERE id=$1`, pending.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.DecideWorkOrder(ctx, pending.ID, "approved"); !errors.Is(err, ErrWorkOrderConflict) {
		t.Fatal("expired request was approved", err)
	}
	rejected, err := pg.CreateWorkOrder(ctx, eventID, "拒绝不必要的复查采样")
	if err != nil {
		t.Fatal(err)
	}
	var rejectedID struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rejected, &rejectedID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.DecideWorkOrder(ctx, rejectedID.ID, "rejected"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pg.BeginWorkOrder(ctx, rejectedID.ID); !errors.Is(err, ErrWorkOrderConflict) {
		t.Fatal("rejected order executed", err)
	}
	closed, err := pg.CreateWorkOrder(ctx, eventID, "事件恢复后不应继续采样")
	if err != nil {
		t.Fatal(err)
	}
	var closedID struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(closed, &closedID); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.DecideWorkOrder(ctx, closedID.ID, "approved"); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE events SET status='resolved',ended_at=now() WHERE id=$1`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := pg.BeginWorkOrder(ctx, closedID.ID); !errors.Is(err, ErrWorkOrderConflict) {
		t.Fatal("resolved event still executed a work order", err)
	}
}

func TestPG18ApproverPasswordRotationAndDisable(t *testing.T) {
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
	defer pg.ApproverPasswordHash(ctx, "")
	first, err := pg.ApproverPasswordHash(ctx, "first-approver-password")
	if err != nil || !security.VerifyPassword("first-approver-password", first) {
		t.Fatal(err)
	}
	second, err := pg.ApproverPasswordHash(ctx, "second-approver-password")
	if err != nil || first == second || !security.VerifyPassword("second-approver-password", second) {
		t.Fatal(err)
	}
	if disabled, err := pg.ApproverPasswordHash(ctx, ""); err != nil || disabled != "" {
		t.Fatal(err)
	}
}
