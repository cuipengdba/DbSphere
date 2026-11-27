package collector

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"time"

	pq "gitcode.com/opengauss/openGauss-connector-go-pq"
	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
)

var (
	ErrOpenGaussVersion  = errors.New("openGauss 7.0.x is required")
	ErrOpenGaussReadOnly = errors.New("openGauss refused the read-only session")
	openGaussVersion     = regexp.MustCompile(`(?i)^\(?openGauss[ \t]+7\.0\.([0-9]{1,2})\b`)
)

const openGaussActivitySource = "pg_catalog.pg_stat_activity"
const openGaussLockSource = "pg_catalog.pg_thread_wait_status"

type OpenGaussWait struct {
	WaitingSession  string `json:"waitingSession"`
	BlockingSession string `json:"blockingSession,omitempty"`
	Event           string `json:"event"`
	Mode            string `json:"mode"`
}

type OpenGaussMetrics struct {
	Version        string          `json:"version"`
	Sessions       *int            `json:"sessions"`
	ActiveSessions *int            `json:"activeSessions"`
	Transactions   *int            `json:"transactions"`
	DatabaseBytes  *int64          `json:"databaseBytes"`
	Waits          []OpenGaussWait `json:"waits"`
	WaitsTruncated bool            `json:"waitsTruncated"`
}

type OpenGaussAdapter struct{}

var _ collectoradapter.Adapter = OpenGaussAdapter{}

func (OpenGaussAdapter) Collect(ctx context.Context, target Target, password string) (collectoradapter.Snapshot, error) {
	config, err := readOnlyPostgresConfig(target, password)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	endpoint := url.URL{Scheme: "postgres", Host: net.JoinHostPort(target.Host, strconv.Itoa(target.Port)),
		Path: "/" + target.Database, User: url.UserPassword(target.Username, password)}
	endpoint.RawPath = "/" + url.PathEscape(target.Database)
	endpoint.RawQuery = "sslmode=disable&connect_timeout=4&target_session_attrs=any&loggerLevel=none"
	native, err := pq.ParseConfig(endpoint.String())
	if err != nil {
		return collectoradapter.Snapshot{}, errors.New("invalid openGauss connection settings")
	}
	native.Password, native.TLSConfig, native.Fallbacks, native.Logger = password, config.TLSConfig, nil, nil
	native.RuntimeParams = map[string]string{"search_path": "pg_catalog", "application_name": "dbsphere-monitor-go",
		"default_transaction_read_only": "on", "statement_timeout": "3000"}
	connector, err := pq.NewConnectorConfig(native)
	if err != nil {
		return collectoradapter.Snapshot{}, errors.New("invalid openGauss connector settings")
	}
	db := sql.OpenDB(connector)
	defer db.Close()
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	defer conn.Close()

	details := OpenGaussMetrics{Waits: []OpenGaussWait{}}
	var readOnly bool
	err = conn.QueryRowContext(ctx, `SELECT version(), current_setting('transaction_read_only')='on'`).Scan(&details.Version, &readOnly)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	version := openGaussVersion.FindStringSubmatch(details.Version)
	if len(version) == 0 {
		return collectoradapter.Snapshot{}, ErrOpenGaussVersion
	}
	patch, _ := strconv.Atoi(version[1])
	versionNum := 70000 + patch
	if !readOnly {
		return collectoradapter.Snapshot{}, ErrOpenGaussReadOnly
	}
	report := collectoradapter.Report{Contract: collectoradapter.ContractVersion, Engine: "opengauss", Product: details.Version,
		Profile: "opengauss-7-single", Version: versionNum, Role: "unknown", Signals: []collectoradapter.Signal{},
		Transport: collectoradapter.Transport{ReadOnly: readOnly, Encrypted: native.TLSConfig != nil, Verified: native.TLSConfig != nil}}
	add := func(key, source string, err error) {
		signal := collectoradapter.Signal{Key: key, Source: source, Status: "ok"}
		if err != nil {
			signal.Status, signal.Reason = "unavailable", "采集查询失败"
			var remote interface{ SQLState() string }
			if errors.As(err, &remote) && remote.SQLState() == "42501" {
				signal.Status, signal.Reason = "permission_denied", "需要只读统计权限"
			}
		}
		report.Signals = append(report.Signals, signal)
	}
	err = conn.QueryRowContext(ctx, `SELECT pg_catalog.pg_database_size(current_database())::bigint`).Scan(&details.DatabaseBytes)
	add("databaseSize", "pg_catalog.pg_database_size", err)

	var monitor bool
	err = conn.QueryRowContext(ctx, `SELECT rolsuper OR rolmonitoradmin FROM pg_catalog.pg_roles WHERE rolname=current_user`).Scan(&monitor)
	if err != nil {
		add("sessions", openGaussActivitySource, err)
		add("lockWaits", openGaussLockSource, err)
	} else if !monitor {
		for _, item := range []struct{ key, source string }{{"sessions", openGaussActivitySource}, {"lockWaits", openGaussLockSource}} {
			report.Signals = append(report.Signals, collectoradapter.Signal{Key: item.key, Source: item.source,
				Status: "permission_denied", Reason: "需要 openGauss MONADMIN 观察其他会话"})
		}
	} else {
		err = conn.QueryRowContext(ctx, `SELECT count(*)::integer,
			coalesce(sum(CASE WHEN state='active' THEN 1 ELSE 0 END),0)::integer, count(xact_start)::integer
			FROM pg_catalog.pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid()`).
			Scan(&details.Sessions, &details.ActiveSessions, &details.Transactions)
		add("sessions", openGaussActivitySource, err)
		details.Waits, details.WaitsTruncated, err = openGaussWaits(ctx, conn)
		add("lockWaits", openGaussLockSource, err)
	}
	return collectoradapter.Snapshot{Report: report, Metrics: map[string]any{
		"opengauss": details, "version_num": versionNum, "role": "unknown",
	}}, nil
}

func openGaussWaits(ctx context.Context, conn *sql.Conn) ([]OpenGaussWait, bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT DISTINCT w.sessionid::text,
		CASE WHEN w.block_sessionid>0 THEN w.block_sessionid::text ELSE '' END,
		coalesce(w.wait_event,'Lock'), w.lockmode
		FROM pg_catalog.pg_thread_wait_status w
		WHERE w.db_name=current_database() AND w.sessionid>0 AND w.lockmode IS NOT NULL AND w.block_sessionid IS NOT NULL
		ORDER BY 1,2,3,4 LIMIT 33`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	waits := []OpenGaussWait{}
	for rows.Next() {
		var wait OpenGaussWait
		if err := rows.Scan(&wait.WaitingSession, &wait.BlockingSession, &wait.Event, &wait.Mode); err != nil {
			return nil, false, err
		}
		waits = append(waits, wait)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return waits[:min(32, len(waits))], len(waits) > 32, nil
}
