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

func TestServiceOperationsPG18(t *testing.T) {
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
	name := "QA service " + time.Now().UTC().Format(time.RFC3339Nano)
	org, err := pg.CreateOrganization(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	scoped := WithAccessActor(WithOrganization(ctx, org.ID), "qa-operator")
	defer pg.pool.Exec(ctx, `DELETE FROM organizations WHERE id=$1`, org.ID)
	created, err := pg.CreateAsset(scoped, assets.Input{Name: name, Host: "127.0.0.1", Port: 5432,
		Database: "postgres", Username: "postgres", SSLMode: "disable", Runtime: "linux", OrganizationID: org.ID},
		[]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	var asset struct{ ID string }
	if err := json.Unmarshal(created, &asset); err != nil || asset.ID == "" {
		t.Fatalf("invalid asset: %s %v", created, err)
	}
	defer pg.DeleteAsset(ctx, asset.ID)
	defer pg.pool.Exec(ctx, `DELETE FROM notification_routes WHERE organization_id=$1`, org.ID)
	defer pg.pool.Exec(ctx, `DELETE FROM service_reports WHERE organization_id=$1`, org.ID)
	if _, err := pg.pool.Exec(ctx, `UPDATE assets SET tags=ARRAY['service:ledger','production'],cluster_name='qa-group' WHERE id=$1`, asset.ID); err != nil {
		t.Fatal(err)
	}
	policy := NotificationRoute{Group: "availability", Initial: []string{"webhook"}, Escalation: []string{"email"}, AfterMinutes: 1}
	if err := pg.SaveNotificationRoute(scoped, policy); err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Truncate(time.Second).Add(-12 * time.Minute)
	check := func(offset int, state string) CheckResult {
		t.Helper()
		metrics := map[string]any{"collector": map[string]any{"pgStatus": state}, "database_bytes": 100000}
		group, reason := "", ""
		if state == "availability" {
			group, reason = state, "QA connection refused"
		}
		point, err := pg.recordCheckAt(ctx, asset.ID, metrics, group, reason, base.Add(time.Duration(offset)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return point
	}
	good := check(0, "ok")
	fault := check(10, "availability")
	check(15, "availability")
	var eventID string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text FROM events WHERE asset_id=$1 AND event_group='availability' AND status='open'`, asset.ID).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if count, err := pg.QueueCriticalEscalations(ctx, base.Add(69*time.Second), 15); err != nil || count != 0 {
		t.Fatalf("early upgrade: %d %v", count, err)
	}
	if count, err := pg.QueueCriticalEscalations(ctx, base.Add(71*time.Second), 15); err != nil || count != 1 {
		t.Fatalf("missing escalation: %d %v", count, err)
	}
	if count, err := pg.QueueCriticalEscalations(ctx, base.Add(72*time.Second), 15); err != nil || count != 0 {
		t.Fatalf("duplicate escalation: %d %v", count, err)
	}
	if _, err := pg.pool.Exec(ctx, `UPDATE events SET acknowledged_at=$2 WHERE id=$1`, eventID, base.Add(25*time.Second)); err != nil {
		t.Fatal(err)
	}
	recovery := check(100, "ok")
	if _, err := pg.pool.Exec(ctx, `INSERT INTO silences(id,kind,asset_id,starts_at,ends_at,reason,created_at)
		VALUES(gen_random_uuid(),'maintenance',$1,$2,$3,'QA maintenance',$2)`, asset.ID, base.Add(20*time.Second), base.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	var deliveries []string
	if err := pg.pool.QueryRow(ctx, `SELECT array_agg(e.kind||':'||n.channel ORDER BY e.kind,n.channel)
		FROM evidence e JOIN notification_outbox n ON n.evidence_id=e.id WHERE e.event_id=$1`, eventID).Scan(&deliveries); err != nil {
		t.Fatal(err)
	}
	wantDeliveries := []string{"alert_escalation:email", "alert_firing:webhook", "alert_recovered:email", "alert_recovered:webhook"}
	if len(deliveries) != len(wantDeliveries) {
		t.Fatalf("deliveries %v", deliveries)
	}
	for i := range wantDeliveries {
		if deliveries[i] != wantDeliveries[i] {
			t.Fatalf("deliveries %v", deliveries)
		}
	}
	query := ServiceReportQuery{From: base.Add(5 * time.Second), To: base.Add(130 * time.Second), Service: "ledger", AssetGroup: "qa-group", Tag: "production"}
	raw, err := pg.ServiceReport(scoped, []string{asset.ID}, query, 10, true)
	if err != nil {
		t.Fatal(err)
	}
	var saved struct {
		ID, Creator string
		Report      struct {
			AvailableSeconds, UnavailableSeconds, MaintenanceSeconds, MissingSeconds float64
			Incidents                                                                []struct {
				ID, TriggerSampleID, RecoverySampleID, LastGoodSampleID         string
				DetectionUpperBoundSeconds, AcknowledgeSeconds, RecoverySeconds float64
			}
		}
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	r := saved.Report
	if saved.Creator != "qa-operator" || r.AvailableSeconds != 25 || r.UnavailableSeconds != 15 || r.MaintenanceSeconds != 10 || r.MissingSeconds != 75 || len(r.Incidents) != 1 {
		t.Fatalf("unexpected report: %s", raw)
	}
	i := r.Incidents[0]
	if i.ID != eventID || i.TriggerSampleID != fault.SampleID || i.RecoverySampleID != recovery.SampleID || i.LastGoodSampleID != good.SampleID ||
		i.DetectionUpperBoundSeconds != 10 || i.AcknowledgeSeconds != 15 || i.RecoverySeconds != 90 {
		t.Fatalf("event evidence: %+v", i)
	}
	if _, err := pg.SavedServiceReport(scoped, []string{}, saved.ID, nil); !errors.Is(err, ErrServiceReportConflict) {
		t.Fatalf("empty scope bypass: %v", err)
	}
	if _, err := pg.SavedServiceReport(WithOrganization(ctx, DefaultOrganizationID), nil, saved.ID, nil); !errors.Is(err, ErrServiceReportConflict) {
		t.Fatalf("organization bypass: %v", err)
	}
	note := "已核对报告与样本"
	if _, err := pg.SavedServiceReport(scoped, []string{asset.ID}, saved.ID, &note); err != nil {
		t.Fatal(err)
	}
	if _, err := pg.SavedServiceReport(scoped, []string{asset.ID}, saved.ID, &note); !errors.Is(err, ErrServiceReportConflict) {
		t.Fatalf("duplicate signoff: %v", err)
	}
	for n, update := range []PostmortemInput{{Action: "draft", Body: "核对触发与恢复"}, {Revision: 1, Action: "reject"},
		{Revision: 2, Action: "draft", Body: "已补充缺采范围"}, {Revision: 3, Action: "archive"}} {
		if _, err := pg.SavePostmortem(scoped, []string{asset.ID}, eventID, update); err != nil {
			t.Fatalf("postmortem step %d: %v", n, err)
		}
	}
	if _, err := pg.SavePostmortem(scoped, []string{asset.ID}, eventID, PostmortemInput{Revision: 3, Action: "draft", Body: "旧版本"}); !errors.Is(err, ErrServiceReportConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	if _, err := pg.ReadPostmortem(scoped, []string{}, eventID); !errors.Is(err, ErrEventNotFound) {
		t.Fatalf("postmortem scope bypass: %v", err)
	}
	if _, err := pg.ReadPostmortem(scoped, []string{asset.ID}, eventID); err != nil {
		t.Fatal(err)
	}
}
