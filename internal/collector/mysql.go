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
	Digest       string   `json:"digest"`
	Calls        uint64   `json:"calls"`
	TotalMS      float64  `json:"totalMs"`
	MeanMS       float64  `json:"meanMs"`
	RowsExamined uint64   `json:"rowsExamined"`
	RowsSent     uint64   `json:"rowsSent"`
	Errors       uint64   `json:"errors"`
	P95MS        *float64 `json:"p95Ms,omitempty"`
	P99MS        *float64 `json:"p99Ms,omitempty"`
	TempDisk     uint64   `json:"tempDiskTables"`
}

type MySQLReplicationChannel struct {
	Name           string                   `json:"name"`
	SourceUUID     string                   `json:"sourceUuid"`
	Receiver       string                   `json:"receiver"`
	Applier        string                   `json:"applier"`
	LastIOError    int                      `json:"lastIoError"`
	RemainingDelay *int64                   `json:"remainingDelaySeconds,omitempty"`
	ReceivedApplied *bool                   `json:"receivedApplied,omitempty"`
	Workers        []MySQLReplicationWorker `json:"workers,omitempty"`
}

type MySQLReplicationWorker struct {
	ID          uint64   `json:"id"`
	State       string   `json:"state"`
	Applying    bool     `json:"applying"`
	LastError   int      `json:"lastError"`
	LastApplyMS *float64 `json:"lastApplyMs,omitempty"`
}

type MySQLTransaction struct {
	Thread    uint64   `json:"thread"`
	Session   *uint64  `json:"session,omitempty"`
	AgeMS     *float64 `json:"ageMs,omitempty"`
	Mode      string   `json:"mode"`
	Isolation string   `json:"isolation"`
}

type MySQLBufferPool struct {
	PageSize      *uint64 `json:"pageSize,omitempty"`
	Pages         *uint64 `json:"pages,omitempty"`
	FreePages     *uint64 `json:"freePages,omitempty"`
	DirtyPages    *uint64 `json:"dirtyPages,omitempty"`
	ReadRequests  *uint64 `json:"readRequests,omitempty"`
	PhysicalReads *uint64 `json:"physicalReads,omitempty"`
	PendingReads  *uint64 `json:"pendingReads,omitempty"`
	PendingWrites *uint64 `json:"pendingWrites,omitempty"`
}

type MySQLMetrics struct {
	Version              string                    `json:"version"`
	ServerUUID           string                    `json:"serverUuid"`
	ServerReadOnly       *bool                     `json:"serverReadOnly,omitempty"`
	Connections          *int                      `json:"connections"`
	Running              *int                      `json:"running"`
	MaxConnections       int                       `json:"maxConnections"`
	LockWaits            []MySQLLockWait           `json:"lockWaits"`
	LocksTruncated       bool                      `json:"locksTruncated"`
	TopSQL               []MySQLDigest             `json:"topSql"`
	Replication          []MySQLReplicationChannel `json:"replication"`
	ReplicationTruncated bool                      `json:"replicationTruncated,omitempty"`
	ActiveTransactions   []MySQLTransaction        `json:"activeTransactions,omitempty"`
	BufferPool           *MySQLBufferPool          `json:"bufferPool,omitempty"`
	UptimeSeconds        *uint64                   `json:"uptimeSeconds,omitempty"`
	Questions            *uint64                   `json:"questions,omitempty"`
}

type MySQLAdapter struct{}

var _ collectoradapter.Adapter = MySQLAdapter{}

func (MySQLAdapter) Collect(ctx context.Context, target Target, password string) (collectoradapter.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	connector, err := mysql.NewConnector(readOnlyMySQLConfig(target, password))
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
	return collectMySQLSnapshot(ctx, conn, target)
}

func readOnlyMySQLConfig(target Target, password string) *mysql.Config {
	config := mysql.NewConfig()
	config.User, config.Passwd = target.Username, password
	config.Net, config.Addr = "tcp", net.JoinHostPort(target.Host, strconv.Itoa(target.Port))
	config.Timeout = 4 * time.Second
	config.ReadTimeout, config.WriteTimeout = 3*time.Second, 3*time.Second
	config.Params = map[string]string{"transaction_read_only": "ON", "max_execution_time": "2000"}
	if target.SSLMode == "require" {
		config.TLSConfig = "true"
	}
	return config
}

func collectMySQLSnapshot(ctx context.Context, conn *sql.Conn, target Target) (collectoradapter.Snapshot, error) {
	details := MySQLMetrics{LockWaits: []MySQLLockWait{}, TopSQL: []MySQLDigest{}, Replication: []MySQLReplicationChannel{}}
	var comment string
	var performanceSchema, serverReadOnly, transactionReadOnly bool
	err := conn.QueryRowContext(ctx, `SELECT @@version, @@version_comment, @@performance_schema,
		(@@global.read_only OR @@global.super_read_only), @@session.transaction_read_only, @@global.max_connections, @@global.server_uuid`).Scan(&details.Version,
		&comment, &performanceSchema, &serverReadOnly, &transactionReadOnly, &details.MaxConnections, &details.ServerUUID)
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
	details.ServerReadOnly = &serverReadOnly
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
	{Key: "innodbBufferPool", Source: "performance_schema.global_status"},
	{Key: "activeTransactions", Source: "performance_schema.events_transactions_current"},
	{Key: "replicaWorkers", Source: "performance_schema.replication_applier_status_by_worker"},
}

func collectMySQLPerformance(ctx context.Context, conn *sql.Conn, schema string, details *MySQLMetrics) []collectoradapter.Signal {
	var err error
	signals := mysqlSignalSources
	err = conn.QueryRowContext(ctx, `SELECT
		MAX(CASE WHEN VARIABLE_NAME='Threads_connected' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END),
		MAX(CASE WHEN VARIABLE_NAME='Threads_running' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END),
		MAX(CASE WHEN VARIABLE_NAME='Questions' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END),
		MAX(CASE WHEN VARIABLE_NAME='Uptime' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END)
		FROM performance_schema.global_status WHERE VARIABLE_NAME IN ('Threads_connected','Threads_running','Questions','Uptime')`).
		Scan(&details.Connections, &details.Running, &details.Questions, &details.UptimeSeconds)
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
		AVG_TIMER_WAIT / 1000000000, SUM_ROWS_EXAMINED,SUM_ROWS_SENT,SUM_ERRORS,
		NULLIF(QUANTILE_95,0) / 1000000000,NULLIF(QUANTILE_99,0) / 1000000000,SUM_CREATED_TMP_DISK_TABLES
		FROM performance_schema.events_statements_summary_by_digest
		WHERE SCHEMA_NAME=? AND DIGEST IS NOT NULL AND COUNT_STAR>0 ORDER BY SUM_TIMER_WAIT DESC,DIGEST LIMIT 15`,
		func(r *sql.Rows, v *MySQLDigest) error {
			return r.Scan(&v.Digest, &v.Calls, &v.TotalMS, &v.MeanMS, &v.RowsExamined, &v.RowsSent, &v.Errors, &v.P95MS, &v.P99MS, &v.TempDisk)
		}, schema)
	signals[2] = mysqlSignalStatus(signals[2], err)

	details.Replication, err = readMySQLRows(ctx, conn, `SELECT r.CHANNEL_NAME, COALESCE(r.SOURCE_UUID,''), r.SERVICE_STATE,
		COALESCE(a.SERVICE_STATE,'UNKNOWN'), r.LAST_ERROR_NUMBER,a.REMAINING_DELAY,
		CASE WHEN @@global.gtid_mode='ON' AND OCTET_LENGTH(r.RECEIVED_TRANSACTION_SET) BETWEEN 1 AND 65536
		AND OCTET_LENGTH(@@global.gtid_executed)<=65536
		THEN GTID_SUBSET(r.RECEIVED_TRANSACTION_SET,@@global.gtid_executed) END
		FROM performance_schema.replication_connection_status r LEFT JOIN performance_schema.replication_applier_status a
		USING (CHANNEL_NAME) ORDER BY r.CHANNEL_NAME LIMIT 21`, func(r *sql.Rows, v *MySQLReplicationChannel) error {
		return r.Scan(&v.Name, &v.SourceUUID, &v.Receiver, &v.Applier, &v.LastIOError, &v.RemainingDelay, &v.ReceivedApplied)
	})
	signals[3] = mysqlSignalStatus(signals[3], err)
	if len(details.Replication) > 20 {
		details.Replication, details.ReplicationTruncated = details.Replication[:20], true
	}
	var pool MySQLBufferPool
	err = conn.QueryRowContext(ctx, `SELECT @@global.innodb_page_size,
		MAX(CASE WHEN VARIABLE_NAME='Innodb_buffer_pool_pages_total' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END),
		MAX(CASE WHEN VARIABLE_NAME='Innodb_buffer_pool_pages_free' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END),
		MAX(CASE WHEN VARIABLE_NAME='Innodb_buffer_pool_pages_dirty' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END),
		MAX(CASE WHEN VARIABLE_NAME='Innodb_buffer_pool_read_requests' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END),
		MAX(CASE WHEN VARIABLE_NAME='Innodb_buffer_pool_reads' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END),
		MAX(CASE WHEN VARIABLE_NAME='Innodb_data_pending_reads' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END),
		MAX(CASE WHEN VARIABLE_NAME='Innodb_data_pending_writes' THEN CAST(VARIABLE_VALUE AS UNSIGNED) END)
		FROM performance_schema.global_status WHERE VARIABLE_NAME LIKE 'Innodb_buffer_pool_%' OR VARIABLE_NAME IN ('Innodb_data_pending_reads','Innodb_data_pending_writes')`).
		Scan(&pool.PageSize, &pool.Pages, &pool.FreePages, &pool.DirtyPages, &pool.ReadRequests, &pool.PhysicalReads, &pool.PendingReads, &pool.PendingWrites)
	if err == nil && pool.Pages != nil {
		details.BufferPool = &pool
	}
	signals[4] = mysqlSignalStatus(signals[4], err)
	if err == nil && details.BufferPool == nil {
		signals[4].Status, signals[4].Reason = "unavailable", "InnoDB buffer pool counters were not returned"
	}
	var consumer bool
	err = conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM performance_schema.setup_consumers
		WHERE NAME='events_transactions_current' AND ENABLED='YES')`).Scan(&consumer)
	if err == nil && consumer {
		details.ActiveTransactions, err = readMySQLRows(ctx, conn, `SELECT t.THREAD_ID,p.PROCESSLIST_ID,
			t.TIMER_WAIT / 1000000000,t.ACCESS_MODE,t.ISOLATION_LEVEL
			FROM performance_schema.events_transactions_current t JOIN performance_schema.threads p USING (THREAD_ID)
			WHERE t.STATE='ACTIVE' AND p.PROCESSLIST_DB=? AND p.PROCESSLIST_ID<>CONNECTION_ID()
			ORDER BY t.TIMER_WAIT DESC,t.THREAD_ID LIMIT 20`, func(r *sql.Rows, v *MySQLTransaction) error {
			return r.Scan(&v.Thread, &v.Session, &v.AgeMS, &v.Mode, &v.Isolation)
		}, schema)
	}
	signals[5] = mysqlSignalStatus(signals[5], err)
	if err == nil && !consumer {
		signals[5].Status, signals[5].Reason = "not_configured", "Transaction event consumer is disabled"
	}
	truncated, err := collectMySQLWorkers(ctx, conn, details.Replication)
	signals[6] = mysqlSignalStatus(signals[6], err)
	if truncated {
		signals[6].Status, signals[6].Reason = "partial", "Replication worker list is capped at 160"
	}
	return signals[:]
}

func collectMySQLWorkers(ctx context.Context, conn *sql.Conn, channels []MySQLReplicationChannel) (bool, error) {
	type workerRow struct {
		name string
		MySQLReplicationWorker
	}
	workers, err := readMySQLRows(ctx, conn, `SELECT CHANNEL_NAME,WORKER_ID,SERVICE_STATE,
		APPLYING_TRANSACTION<>'',LAST_ERROR_NUMBER,
		CASE WHEN YEAR(LAST_APPLIED_TRANSACTION_START_APPLY_TIMESTAMP)>0 THEN TIMESTAMPDIFF(MICROSECOND,
			LAST_APPLIED_TRANSACTION_START_APPLY_TIMESTAMP,LAST_APPLIED_TRANSACTION_END_APPLY_TIMESTAMP)/1000 END
		FROM performance_schema.replication_applier_status_by_worker ORDER BY CHANNEL_NAME,WORKER_ID LIMIT 161`,
		func(row *sql.Rows, worker *workerRow) error {
			return row.Scan(&worker.name, &worker.ID, &worker.State, &worker.Applying, &worker.LastError, &worker.LastApplyMS)
		})
	truncated := len(workers) > 160
	if truncated {
		workers = workers[:160]
	}
	byChannel := make(map[string][]MySQLReplicationWorker)
	for _, worker := range workers {
		byChannel[worker.name] = append(byChannel[worker.name], worker.MySQLReplicationWorker)
	}
	for i := range channels {
		channels[i].Workers = byChannel[channels[i].Name]
	}
	return truncated, err
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
