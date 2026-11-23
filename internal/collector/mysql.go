package collector

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/go-sql-driver/mysql"
)

var mysql84Version = regexp.MustCompile(`^8\.4\.(\d+)(?:[-+]|$)`)

var ErrMySQLVersion = errors.New("MySQL monitoring requires MySQL 8.4 LTS")
var ErrMySQLReadOnly = errors.New("MySQL monitoring session is not read-only")

type MySQLLockWait struct {
	WaitingThread  uint64 `json:"waitingThread"`
	BlockingThread uint64 `json:"blockingThread"`
	Schema         string `json:"schema"`
	Table          string `json:"table"`
	Mode           string `json:"mode"`
}

type MySQLDigest struct {
	Digest       string  `json:"digest"`
	Calls        uint64  `json:"calls"`
	TotalMS      float64 `json:"totalMs"`
	MeanMS       float64 `json:"meanMs"`
	RowsExamined uint64  `json:"rowsExamined"`
}

type MySQLReplicationChannel struct {
	Name        string `json:"name"`
	SourceUUID  string `json:"sourceUuid"`
	Receiver    string `json:"receiver"`
	Applier     string `json:"applier"`
	LastIOError int    `json:"lastIoError"`
}

type MySQLMetrics struct {
	Version        string                    `json:"version"`
	Connections    *int                      `json:"connections"`
	Running        *int                      `json:"running"`
	MaxConnections int                       `json:"maxConnections"`
	LockWaits      []MySQLLockWait           `json:"lockWaits"`
	LocksTruncated bool                      `json:"locksTruncated"`
	TopSQL         []MySQLDigest             `json:"topSql"`
	Replication    []MySQLReplicationChannel `json:"replication"`
}

type MySQLAdapter struct{}

var _ collectoradapter.Adapter = MySQLAdapter{}

func (MySQLAdapter) Collect(ctx context.Context, target Target, password string) (collectoradapter.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	config := mysql.NewConfig()
	config.User, config.Passwd = target.Username, password
	config.Net, config.Addr = "tcp", net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
	config.Timeout = 4 * time.Second
	config.ReadTimeout, config.WriteTimeout = 3*time.Second, 3*time.Second
	config.Params = map[string]string{"transaction_read_only": "ON", "max_execution_time": "2000"}
	if target.SSLMode == "require" {
		config.TLSConfig = "true"
	}
	connector, err := mysql.NewConnector(config)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	database := sql.OpenDB(connector)
	defer database.Close()
	conn, err := database.Conn(ctx)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	defer conn.Close()
	details := MySQLMetrics{LockWaits: []MySQLLockWait{}, TopSQL: []MySQLDigest{}, Replication: []MySQLReplicationChannel{}}
	var comment string
	var performanceSchema, serverReadOnly, transactionReadOnly bool
	err = conn.QueryRowContext(ctx, `SELECT @@version, @@version_comment, @@performance_schema,
		@@global.read_only, @@session.transaction_read_only, @@global.max_connections`).Scan(&details.Version,
		&comment, &performanceSchema, &serverReadOnly, &transactionReadOnly, &details.MaxConnections)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	parts := mysql84Version.FindStringSubmatch(details.Version)
	if len(parts) != 2 || !strings.Contains(strings.ToLower(comment), "mysql") {
		return collectoradapter.Snapshot{}, ErrMySQLVersion
	}
	patch, err := strconv.Atoi(parts[1])
	if err != nil || patch > 99 {
		return collectoradapter.Snapshot{}, ErrMySQLVersion
	}
	if !transactionReadOnly {
		return collectoradapter.Snapshot{}, ErrMySQLReadOnly
	}
	report := collectoradapter.Report{Contract: collectoradapter.ContractVersion, Engine: "mysql", Profile: "mysql-8.4",
		Version: 80400 + patch, Role: "writable", Transport: collectoradapter.Transport{ReadOnly: true}}
	if serverReadOnly {
		report.Role = "read_only"
	}
	if target.SSLMode == "require" {
		report.Transport.Encrypted, report.Transport.Verified = true, true
		var name string
		_ = conn.QueryRowContext(ctx, `SHOW SESSION STATUS LIKE 'Ssl_version'`).Scan(&name, &report.Transport.Protocol)
	}
	if performanceSchema {
		report.Signals = collectMySQLPerformance(ctx, conn, target.Database, &details)
		if len(details.Replication) > 0 {
			report.Role = "replica"
		}
	} else {
		for _, signal := range mysqlSignalSources {
			signal.Status, signal.Reason = "not_configured", "Performance Schema is disabled"
			report.Signals = append(report.Signals, signal)
		}
	}
	metrics := map[string]any{"mysql": details, "version_num": report.Version, "role": report.Role}
	if details.Connections != nil {
		metrics["connections"], metrics["max_connections"] = *details.Connections, details.MaxConnections
	}
	return collectoradapter.Snapshot{Metrics: metrics, Report: report}, nil
}

var mysqlSignalSources = [...]collectoradapter.Signal{
	{Key: "connections", Source: "performance_schema.global_status"},
	{Key: "lockWaits", Source: "performance_schema.data_lock_waits / data_locks"},
	{Key: "sqlPerformance", Source: "performance_schema.events_statements_summary_by_digest"},
	{Key: "replication", Source: "performance_schema.replication_connection_status / replication_applier_status"},
}

func collectMySQLPerformance(ctx context.Context, conn *sql.Conn, schema string, details *MySQLMetrics) []collectoradapter.Signal {
	var err error
	signals := mysqlSignalSources
	err = conn.QueryRowContext(ctx, `SELECT
		MAX(CASE WHEN VARIABLE_NAME='Threads_connected' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END),
		MAX(CASE WHEN VARIABLE_NAME='Threads_running' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END)
		FROM performance_schema.global_status WHERE VARIABLE_NAME IN ('Threads_connected','Threads_running')`).Scan(&details.Connections, &details.Running)
	signals[0] = mysqlSignalStatus(signals[0], err)
	if err == nil && details.Connections == nil {
		signals[0].Status, signals[0].Reason = "unavailable", "Connection counters were not returned"
	}

	details.LockWaits, err = readMySQLRows(ctx, conn, `SELECT COALESCE(w.REQUESTING_THREAD_ID,0), COALESCE(w.BLOCKING_THREAD_ID,0),
		COALESCE(l.OBJECT_SCHEMA,''), COALESCE(l.OBJECT_NAME,''), COALESCE(l.LOCK_MODE,'')
		FROM performance_schema.data_lock_waits w LEFT JOIN performance_schema.data_locks l
		ON l.ENGINE=w.ENGINE AND l.ENGINE_LOCK_ID=w.REQUESTING_ENGINE_LOCK_ID
		WHERE w.ENGINE='INNODB' LIMIT 21`, func(r *sql.Rows, v *MySQLLockWait) error {
		return r.Scan(&v.WaitingThread, &v.BlockingThread, &v.Schema, &v.Table, &v.Mode)
	})
	signals[1] = mysqlSignalStatus(signals[1], err)
	if len(details.LockWaits) > 20 {
		details.LockWaits, details.LocksTruncated = details.LockWaits[:20], true
	}

	details.TopSQL, err = readMySQLRows(ctx, conn, `SELECT DIGEST, COUNT_STAR, SUM_TIMER_WAIT / 1000000000,
		AVG_TIMER_WAIT / 1000000000, SUM_ROWS_EXAMINED FROM performance_schema.events_statements_summary_by_digest
		WHERE SCHEMA_NAME=? AND DIGEST IS NOT NULL AND COUNT_STAR>0 ORDER BY SUM_TIMER_WAIT DESC,DIGEST LIMIT 15`,
		func(r *sql.Rows, v *MySQLDigest) error {
			return r.Scan(&v.Digest, &v.Calls, &v.TotalMS, &v.MeanMS, &v.RowsExamined)
		}, schema)
	signals[2] = mysqlSignalStatus(signals[2], err)

	details.Replication, err = readMySQLRows(ctx, conn, `SELECT r.CHANNEL_NAME, COALESCE(r.SOURCE_UUID,''), r.SERVICE_STATE,
		COALESCE(a.SERVICE_STATE,'UNKNOWN'), r.LAST_ERROR_NUMBER
		FROM performance_schema.replication_connection_status r LEFT JOIN performance_schema.replication_applier_status a
		USING (CHANNEL_NAME) ORDER BY r.CHANNEL_NAME LIMIT 20`, func(r *sql.Rows, v *MySQLReplicationChannel) error {
		return r.Scan(&v.Name, &v.SourceUUID, &v.Receiver, &v.Applier, &v.LastIOError)
	})
	signals[3] = mysqlSignalStatus(signals[3], err)
	return signals[:]
}

func readMySQLRows[T any](ctx context.Context, conn *sql.Conn, query string, scan func(*sql.Rows, *T) error, args ...any) ([]T, error) {
	items := []T{}
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return items, err
	}
	defer rows.Close()
	for rows.Next() {
		var item T
		if err := scan(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func mysqlSignalStatus(signal collectoradapter.Signal, err error) collectoradapter.Signal {
	signal.Status = "ok"
	if err != nil {
		signal.Status, signal.Reason = "unavailable", "Performance Schema query failed"
		var remote *mysql.MySQLError
		if errors.As(err, &remote) {
			signal.Reason = "MySQL error " + strconv.Itoa(int(remote.Number))
			if remote.Number == 1142 || remote.Number == 1044 || remote.Number == 1227 {
				signal.Status = "limited_permissions"
			}
		}
	}
	return signal
}
