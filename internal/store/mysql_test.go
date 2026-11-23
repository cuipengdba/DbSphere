package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/go-sql-driver/mysql"
)

func TestLiveMySQL84PreviewLockReplay(t *testing.T) {
	postgresURL, mysqlDSN := os.Getenv("DBSPHERE_TEST_MYSQL_PG18_URL"), os.Getenv("DBSPHERE_TEST_MYSQL84_DSN")
	if postgresURL == "" || mysqlDSN == "" {
		t.Skip("dedicated MySQL 8.4 and PostgreSQL 18 acceptance fixtures are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	admin, err := sql.Open("mysql", mysqlDSN)
	if err != nil {
		t.Fatal("invalid MySQL fixture DSN")
	}
	defer admin.Close()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	schema := "dbsphere_mysql84_" + suffix
	monitor := "dbsphere_ci_" + suffix
	account := "'" + monitor + "'@'%'"
	const grantLocks = `GRANT SELECT ON performance_schema.data_lock_waits TO `
	for _, statement := range []string{
		`CREATE DATABASE ` + schema,
		`CREATE TABLE ` + schema + `.orders (id int PRIMARY KEY, n int) ENGINE=InnoDB`,
		`INSERT INTO ` + schema + `.orders VALUES(1,0)`,
		`CREATE USER ` + account + ` IDENTIFIED BY 'dbsphere-ci-monitor'`,
		`GRANT SELECT ON performance_schema.global_status TO ` + account,
		`GRANT SELECT ON performance_schema.data_locks TO ` + account,
		grantLocks + account,
		`GRANT SELECT ON performance_schema.events_statements_summary_by_digest TO ` + account,
	} {
		if _, err := admin.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare MySQL fixture: %v", err)
		}
	}
	store, err := Open(ctx, postgresURL)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config, err := mysql.ParseDSN(mysqlDSN)
	if err != nil {
		t.Fatal("invalid MySQL fixture DSN")
	}
	host, rawPort, err := net.SplitHostPort(config.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(rawPort)
	key := []byte("01234567890123456789012345678901")
	input := assets.Input{Name: "MySQL 8.4 CI", Host: host, Port: port, Database: schema,
		Username: monitor, Password: "dbsphere-ci-monitor", SSLMode: "disable", Runtime: assets.RuntimeMySQL}
	if err := input.Validate(); err != nil {
		t.Fatal(err)
	}
	asset, err := store.CreateAsset(ctx, input, key)
	if err != nil {
		t.Fatal(err)
	}
	var access struct{ ID, Engine string }
	if err := json.Unmarshal(asset, &access); err != nil || access.Engine != "mysql" {
		t.Fatal("missing MySQL engine on asset", err)
	}
	due, err := store.DueAssetIDs(ctx, time.Minute, 50)
	if err != nil || !slices.Contains(due, access.ID) {
		t.Fatal("MySQL asset is missing from the regular collection schedule", err)
	}
	holder, err := admin.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Rollback()
	update := `UPDATE ` + schema + `.orders SET n=n+1 WHERE id=1`
	if _, err := holder.ExecContext(ctx, update); err != nil {
		t.Fatal(err)
	}
	waiter := make(chan error, 1)
	go func() { _, err := admin.ExecContext(ctx, update); waiter <- err }()
	for {
		var count int
		if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM performance_schema.data_lock_waits`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	check := func() CheckResult {
		t.Helper()
		result, err := store.CheckAsset(ctx, access.ID, key)
		if err != nil || result.Status != "ok" {
			t.Fatalf("MySQL collection: %+v %v", result, err)
		}
		return result
	}
	blocked := check()
	check()
	var eventID string
	if err := store.pool.QueryRow(ctx, `SELECT event_id::text FROM open_event_groups WHERE asset_id=$1 AND event_group=$2`, access.ID, mysqlLockGroup).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.ExecContext(ctx, `REVOKE SELECT ON performance_schema.data_lock_waits FROM `+account); err != nil {
		t.Fatal(err)
	}
	check()
	var opened bool
	if err := store.pool.QueryRow(ctx, `SELECT status='open' FROM events WHERE id=$1`, eventID).Scan(&opened); err != nil || !opened {
		t.Fatal("missing lock permission must preserve the original open incident", err)
	}
	if _, err := admin.ExecContext(ctx, grantLocks+account); err != nil {
		t.Fatal(err)
	}
	if err := holder.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-waiter; err != nil {
		t.Fatal(err)
	}
	recovered := check()
	event, err := store.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(event)
	if err != nil || !strings.Contains(string(encoded), `"status":"resolved"`) ||
		!strings.Contains(string(encoded), `"code":"mysql_innodb_lock_wait"`) ||
		!strings.Contains(string(encoded), `"evidenceStrength":"direct"`) ||
		!strings.Contains(string(encoded), blocked.SampleID) || !strings.Contains(string(encoded), recovered.SampleID) {
		t.Fatal("MySQL event must preserve detection, blocker analysis and recovery samples", err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE kind=$2 AND asset_id=$1`, access.ID, mysqlLockGroup).Scan(&count); err != nil || count != 1 {
		t.Fatalf("observed %d MySQL lock events, expected one: %v", count, err)
	}
}
