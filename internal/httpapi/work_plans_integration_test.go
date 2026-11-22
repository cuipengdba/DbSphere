package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/cuipengdba/dbsphere/internal/store"
	"github.com/jackc/pgx/v5"
)

func TestPG18ControlledPlansWithIndependentTargets(t *testing.T) {
	sourceURL, restoredURL := os.Getenv("DBSPHERE_TEST_PG18_URL"), os.Getenv("DBSPHERE_TEST_PG18_RESTORE_URL")
	if sourceURL == "" || restoredURL == "" {
		t.Skip("Two dedicated PostgreSQL 18 fixtures are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	source, err := pgx.Connect(ctx, sourceURL)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close(context.Background())
	restored, err := pgx.Connect(ctx, restoredURL)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close(context.Background())
	if source.Config().Host == restored.Config().Host && source.Config().Port == restored.Config().Port || source.Config().Database == restored.Config().Database {
		t.Fatal("Restore test must use a distinct PostgreSQL endpoint and database")
	}
	pg, err := store.Open(ctx, sourceURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()

	suffix := fmt.Sprintf("%x", time.Now().UnixNano())
	relation := "work_targets_" + suffix
	actor := "job_exec_" + suffix
	table := pgx.Identifier{"public", relation}.Sanitize()
	role := pgx.Identifier{actor}.Sanitize()
	const pass = "demo-executor-test-only"
	for _, fixture := range []*pgx.Conn{source, restored} {
		if _, err := fixture.Exec(ctx, "CREATE ROLE "+role+" LOGIN PASSWORD '"+pass+"'"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Connections may already have closed on an early failure.
			cleanCtx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			cleanup, err := pgx.ConnectConfig(cleanCtx, fixture.Config())
			if err != nil {
				t.Error(err)
				return
			}
			defer cleanup.Close(cleanCtx)
			if _, err := cleanup.Exec(cleanCtx, "DROP TABLE IF EXISTS "+table+"; DROP ROLE IF EXISTS "+role); err != nil {
				t.Error(err)
			}
		})
		if _, err := fixture.Exec(ctx, "CREATE TABLE "+table+" (marker text) WITH (autovacuum_enabled=false)"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := source.Exec(ctx, "INSERT INTO "+table+" VALUES ('before-cutoff'),('after-cutoff'); GRANT MAINTAIN ON "+table+" TO "+role); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.Exec(ctx, "INSERT INTO "+table+" VALUES ('before-cutoff'); GRANT SELECT(marker) ON "+table+" TO "+role); err != nil {
		t.Fatal(err)
	}

	key := []byte("01234567890123456789012345678901")
	cfg := source.Config()
	assetRaw, err := pg.CreateAsset(ctx, assets.Input{Name: "controlled work drill " + suffix, Host: cfg.Host, Port: int(cfg.Port), Database: cfg.Database,
		Username: "missing_monitor_" + suffix, Password: "synthetic-monitor-secret", SSLMode: "disable", Runtime: "linux"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct{ ID string }
	if err := json.Unmarshal(assetRaw, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(context.Background(), asset.ID)
	if result, err := pg.CheckAsset(ctx, asset.ID, key); err != nil || result.Status != "error" {
		t.Fatal("Expected a failed monitor sample", err)
	}
	var event string
	if err := source.QueryRow(ctx, "SELECT id::text FROM events WHERE asset_id=$1 AND status='open'", asset.ID).Scan(&event); err != nil {
		t.Fatal(err)
	}

	dsn := func(c *pgx.ConnConfig) string {
		return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", actor, pass, c.Host, c.Port, c.Database)
	}
	adminHash, err := security.HashPassword("work-plans-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	approverHash, err := security.HashPassword("work-plans-approve-password")
	if err != nil {
		t.Fatal(err)
	}
	profiles := map[string]WorkOrderProfile{
		"maintenance": {AssetID: asset.ID, Action: store.WorkAnalyze, DSN: dsn(cfg), Schema: "public", Relation: relation},
		"isolated": {AssetID: asset.ID, Action: store.WorkVerifyRestore, DSN: dsn(restored.Config()), Schema: "public", Relation: relation,
			MarkerColumn: "marker", RequiredMarker: "before-cutoff", ExcludedMarker: "after-cutoff"},
	}
	handler := New(pg, AuthConfig{MasterKey: key, PasswordHash: adminHash, ApproverPasswordHash: approverHash, WorkOrderProfiles: profiles})
	type session struct {
		cookie *http.Cookie
		csrf   string
	}
	call := func(method, path string, body any, auth session, codes ...int) *httptest.ResponseRecorder {
		t.Helper()
		var content string
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			content = string(encoded)
		}
		request := httptest.NewRequest(method, path, strings.NewReader(content)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		if auth.cookie != nil {
			request.AddCookie(auth.cookie)
			request.Header.Set("X-CSRF-Token", auth.csrf)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, request)
		for _, code := range codes {
			if res.Code == code {
				return res
			}
		}
		t.Fatalf("%s %s got %d, expected %v: %s", method, path, res.Code, codes, res.Body.String())
		return nil
	}
	login := func(password string) session {
		res := call("POST", "/api/login", map[string]string{"password": password}, session{}, 200)
		var login struct{ CSRF string }
		if err := json.Unmarshal(res.Body.Bytes(), &login); err != nil {
			t.Fatal(err)
		}
		return session{res.Result().Cookies()[0], login.CSRF}
	}
	admin, reviewer := login("work-plans-admin-password"), login("work-plans-approve-password")
	path := "/api/events/" + event + "/work-orders"
	type orderResult struct {
		ID, Action, Status string
		Result             map[string]any
	}
	orderFrom := func(w *httptest.ResponseRecorder) orderResult {
		t.Helper()
		var payload struct{ Order orderResult }
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil || payload.Order.ID == "" {
			t.Fatalf("work order response: %v %s", err, w.Body.String())
		}
		return payload.Order
	}
	submit := func(in store.WorkOrderInput) orderResult { return orderFrom(call("POST", path, in, admin, 201)) }
	orderPath := func(id, action string) string { return "/api/work-orders/" + id + "/" + action }
	approve := func(id string) { call("POST", orderPath(id, "approve"), nil, reviewer, 200) }
	execute := func(id string) orderResult { return orderFrom(call("POST", orderPath(id, "execute"), nil, admin, 200)) }

	analyze := store.WorkOrderInput{Action: store.WorkAnalyze, Profile: "maintenance", IdempotencyKey: "stats-1", Reason: "Recalculate fixture table statistics"}
	res := call("POST", path+"/preview", analyze, admin, 200)
	if !strings.Contains(res.Body.String(), relation) || strings.Contains(res.Body.String(), pass) {
		t.Fatalf("Unexpected preview: %s", res.Body.String())
	}
	var analyses int
	countStats := func() {
		t.Helper()
		if err := source.QueryRow(ctx, "SELECT analyze_count FROM pg_stat_user_tables WHERE relid=to_regclass($1)", table).Scan(&analyses); err != nil {
			t.Fatal(err)
		}
	}
	countStats()
	if analyses != 0 {
		t.Fatal("Dry run wrote PostgreSQL statistics")
	}
	first := submit(analyze)
	if replay := orderFrom(call("POST", path, analyze, admin, 200)); replay.ID != first.ID {
		t.Fatal("Duplicate request created a second job")
	}
	conflict := analyze
	conflict.Reason = "Reused key with a different request"
	call("POST", path, conflict, admin, 409)
	call("POST", orderPath(first.ID, "execute"), nil, admin, 409)
	approve(first.ID)
	done := execute(first.ID)
	if done.Status != "completed" || done.Result["relation"] != relation {
		t.Fatalf("ANALYZE result: %+v", done)
	}
	call("POST", orderPath(first.ID, "execute"), nil, admin, 409)
	for i := 0; i < 20; i++ {
		_, _ = source.Exec(ctx, "SELECT pg_stat_clear_snapshot()")
		countStats()
		if analyses == 1 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if analyses != 1 {
		t.Fatal("PostgreSQL did not report exactly one ANALYZE", analyses)
	}

	verify := store.WorkOrderInput{Action: store.WorkVerifyRestore, Profile: "isolated", IdempotencyKey: "restore-1", Reason: "Check data on isolated restore target"}
	call("POST", path+"/preview", verify, admin, 200)
	recovery := submit(verify)
	approve(recovery.ID)
	recovered := execute(recovery.ID)
	if recovered.Status != "completed" || recovered.Result["expectedPresent"] != true || recovered.Result["laterMarkerAbsent"] != true {
		t.Fatalf("Recovery markers were not verified: %+v", recovered)
	}
	verify.IdempotencyKey, verify.TimeoutSeconds = "restore-timeout", 1
	late := submit(verify)
	approve(late.ID)
	tx, err := restored.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	timedOut := execute(late.ID)
	if timedOut.Status != "failed" || timedOut.Result["errorCode"] != "execution_timeout" {
		t.Fatalf("Timeout not archived: %+v", timedOut)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	inspection := store.WorkOrderInput{Action: store.WorkInspect, TemplateID: "pg_daily", IdempotencyKey: "inspect-1", Reason: "Create a scoped incident report"}
	call("POST", path+"/preview", inspection, admin, 200)
	denied := submit(inspection)
	call("POST", orderPath(denied.ID, "reject"), nil, reviewer, 200)
	call("POST", orderPath(denied.ID, "execute"), nil, admin, 409)
	inspection.IdempotencyKey = "inspect-expired"
	expired := submit(inspection)
	if _, err := source.Exec(ctx, "UPDATE work_orders SET expires_at=now()-interval '1 second' WHERE id=$1", expired.ID); err != nil {
		t.Fatal(err)
	}
	call("POST", orderPath(expired.ID, "approve"), nil, reviewer, 409)
	inspection.IdempotencyKey = "inspect-verified"
	report := submit(inspection)
	approve(report.ID)
	inspected := execute(report.ID)
	if inspected.Status != "completed" || inspected.Result["inspectionRunId"] == "" || inspected.Result["sampleId"] == "" {
		t.Fatalf("Inspection references missing: %+v", inspected)
	}
	call("GET", "/api/inspection-runs/"+inspected.Result["inspectionRunId"].(string)+"/report", nil, admin, 200)
	evidence := call("GET", "/api/events/"+event, nil, admin, 200).Body.String()
	for _, id := range []string{first.ID, recovery.ID, late.ID, report.ID} {
		if !strings.Contains(evidence, id) {
			t.Fatal("Job evidence missing from original event", id)
		}
	}
	if strings.Contains(evidence, pass) {
		t.Fatal("Execution secret in incident evidence")
	}
	t.Logf("incident=%s analyze=%s recovery=%s timeout=%s inspection=%s", event, first.ID, recovery.ID, late.ID, report.ID)
}
