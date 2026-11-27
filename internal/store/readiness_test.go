package store

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collector"
	"github.com/jackc/pgx/v5"
)

func TestReadinessSeparatesPVCVisibilityFromDatabaseHealth(t *testing.T) {
	sample := readinessSample{Status: "ok", Metrics: map[string]any{
		"collector": map[string]any{"pgStatus": "ok"},
		"k8s": map[string]any{"status": "ok", "node": map[string]any{"uid": "node-uid"},
			"pvcs": []any{map[string]any{"status": "unavailable", "error": "Kubernetes API HTTP 403"}}},
	}}
	checks := describeReadiness(sample, "k8s")
	for key, want := range map[string]string{"connection": "ok", "k8s": "ok", "k8sNode": "ok", "k8sPVC": "limited"} {
		if !slices.ContainsFunc(checks, func(check readinessCheck) bool { return check.Key == key && check.Status == want }) {
			t.Errorf("missing %s → %s: %+v", key, want, checks)
		}
	}
	if signal := readGoSignal("collector.pg_stats_limited", sample.Metrics, time.Now()); signal.Available {
		t.Fatal("an unknown SQL permission cannot resolve the permission incident")
	}
}

func TestReadinessPreservesConnectionPermissionLimit(t *testing.T) {
	for _, tc := range []struct{ message, want string }{
		{"Redis cannot read INFO server", "limited"},
		{"PostgreSQL monitoring role lacks permission (SQLSTATE 42501)", "limited"},
		{"PostgreSQL authentication failed (SQLSTATE 28P01)", "failed"},
	} {
		checks := describeReadiness(readinessSample{Status: "error", Error: tc.message}, "redis")
		if status := summarizeReadiness(checks); status != tc.want {
			t.Errorf("%s: got %s, want %s", tc.message, status, tc.want)
		}
	}
}

func TestDomesticDatabaseReadinessDistinguishesPermissionFromObservedZero(t *testing.T) {
	for _, scenario := range []struct{ runtime, statusKey, signal, source string }{
		{"linux", "pgStatus", "activeWaits", "pg_catalog.pg_stat_activity"},
		{"dm8", "dm8Status", "lockWaits", "SYS.V$TRXWAIT"},
		{"kingbase", "kingbaseStatus", "lockWaits", "sys_catalog.sys_locks"},
		{"opengauss", "opengaussStatus", "lockWaits", "pg_catalog.pg_thread_wait_status"},
	} {
		t.Run(scenario.runtime, func(t *testing.T) {
			for _, permission := range []struct{ signalStatus, checkStatus, overall string }{
				{"permission_denied", "limited", "limited"},
				{"unavailable", "unknown", "limited"},
				{"ok", "ok", "ready"},
			} {
				sample := readinessSample{Status: "ok", Metrics: map[string]any{
					"collector": map[string]any{scenario.statusKey: "ok"},
					"adapter": map[string]any{"transport": map[string]any{"readOnly": true}, "signals": []any{
						map[string]any{"key": scenario.signal, "status": permission.signalStatus, "source": scenario.source},
					}},
				}}
				checks := describeReadiness(sample, scenario.runtime)
				if summarizeReadiness(checks) != permission.overall || !slices.ContainsFunc(checks, func(check readinessCheck) bool {
					return check.Key == scenario.signal && check.Status == permission.checkStatus
				}) {
					t.Fatalf("%s: unexpected checks %+v", permission.signalStatus, checks)
				}
			}
		})
	}
}

func TestKindReadinessReportsDeniedPVC(t *testing.T) {
	namespace, pod := os.Getenv("DBSPHERE_TEST_READINESS_NAMESPACE"), os.Getenv("DBSPHERE_TEST_READINESS_POD")
	if namespace == "" || pod == "" {
		t.Skip("isolated Kubernetes Pod with PVC and a Pod-only reader required")
	}
	kube := collector.Kubernetes(t.Context(), collector.KubernetesTarget{Namespace: namespace, Pod: pod})
	found := slices.ContainsFunc(list(kube["pvcs"]), func(raw any) bool {
		return object(raw)["error"] == "Kubernetes API HTTP 403"
	})
	if kube["status"] != "ok" || !found {
		t.Fatal("the live reader must be able to observe the Pod and receive HTTP 403 for its claim")
	}
	coverage := describeReadiness(readinessSample{Metrics: map[string]any{
		"collector": map[string]any{"pgStatus": "ok"}, "k8s": kube,
	}}, "k8s")
	for key, want := range map[string]string{"connection": "ok", "k8s": "ok", "k8sPVC": "limited"} {
		if !slices.ContainsFunc(coverage, func(check readinessCheck) bool { return check.Key == key && check.Status == want }) {
			t.Fatalf("missing live result %s → %s: %+v", key, want, coverage)
		}
	}
}

func TestPG18ReadinessChangesRecoverTheSameEvent(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if dsn == "" {
		t.Skip("isolated PG18 fixture required")
	}
	ctx := t.Context()
	credential, err := pgx.ParseConfig(dsn)
	if err != nil || credential.Host == "" || credential.Host[0] == '/' {
		t.Fatal("a TCP PG18 fixture is required")
	}
	pg, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	role := fmt.Sprintf("dbsphere_ready_%x", time.Now().UnixNano())
	identifier := pgx.Identifier{role}.Sanitize()
	const password = "readiness-fixture-only-220"
	if _, err := pg.pool.Exec(ctx, "CREATE ROLE "+identifier+" LOGIN PASSWORD '"+password+"'"); err != nil {
		t.Fatal(err)
	}
	defer pg.pool.Exec(ctx, "DROP ROLE "+identifier)
	grant := func(allow bool) {
		t.Helper()
		sql := "GRANT pg_read_all_stats TO "
		if !allow {
			sql = "REVOKE pg_read_all_stats FROM "
		}
		if _, err := pg.pool.Exec(ctx, sql+identifier); err != nil {
			t.Fatal(err)
		}
	}
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "coverage " + role,
		Host: credential.Host, Port: int(credential.Port), Database: credential.Database,
		Username: role, Password: password, Runtime: "linux", SSLMode: "disable"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct{ ID string }
	if err := json.Unmarshal(created, &asset); err != nil || asset.ID == "" {
		t.Fatalf("fixture asset: %v", err)
	}
	defer pg.DeleteAsset(ctx, asset.ID)
	check := func(waitStatus string) readinessReport {
		t.Helper()
		result, err := pg.CheckAsset(ctx, asset.ID, key)
		if err != nil || result.Status != "ok" {
			t.Fatalf("the database sample should remain healthy: %+v %v", result, err)
		}
		report, err := pg.assetReadiness(ctx, asset.ID, nil)
		if err != nil || report.SampleID != result.SampleID || report.SampledAt == nil || report.VersionNum/10000 != 18 ||
			!slices.ContainsFunc(report.Checks, func(c readinessCheck) bool { return c.Key == "activeWaits" && c.Status == waitStatus }) {
			t.Fatalf("incorrect current coverage: %+v %v", report, err)
		}
		return report
	}
	grant(true)
	baseline := check("ok")
	grant(false)
	limited := check("limited")
	if limited.PreviousSampleID != baseline.SampleID {
		t.Fatal("permission changes must reference the previous observation")
	}
	var eventID, state string
	if err := pg.pool.QueryRow(ctx, `SELECT id::text,status FROM events
		WHERE asset_id=$1 AND event_group='telemetry' ORDER BY started_at DESC LIMIT 1`, asset.ID).
		Scan(&eventID, &state); err != nil || state != "open" {
		t.Fatalf("expected a telemetry incident: %s %v", state, err)
	}
	grant(true)
	recovery := check("ok")
	if recovery.PreviousSampleID != limited.SampleID {
		t.Fatal("lost the original permission failure sample")
	}
	if err := pg.pool.QueryRow(ctx, "SELECT status FROM events WHERE id=$1", eventID).Scan(&state); err != nil || state != "resolved" {
		t.Fatalf("the same event did not recover: %s %v", state, err)
	}
}
