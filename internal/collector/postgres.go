package collector

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/cuipengdba/dbsphere/pkg/collectoradapter"
	"github.com/jackc/pgx/v5"
)

type Target = collectoradapter.Target

const (
	minPostgresMajor = 14
	maxPostgresMajor = 19
)

func PostgresMajorSupported(major int) bool {
	return major >= minPostgresMajor && major <= maxPostgresMajor
}

func readOnlyPostgresConfig(target Target, password string) (*pgx.ConnConfig, error) {
	mode := "disable"
	if target.SSLMode == "require" {
		mode = "verify-full"
	}
	profile := postgresAdapterProfile(target)
	if profile == "aws-rds-postgresql" && mode != "verify-full" {
		return nil, fmt.Errorf("AWS RDS PostgreSQL requires verified TLS")
	}
	address := url.URL{Scheme: "postgres", Host: net.JoinHostPort(target.Host, strconv.Itoa(target.Port)),
		Path: "/" + target.Database, User: url.UserPassword(target.Username, password)}
	params := url.Values{"sslmode": {mode}}
	var pem64 string
	if mode == "verify-full" {
		file := os.Getenv("DBSPHERE_POSTGRES_CA_FILE")
		pem64 = os.Getenv("DBSPHERE_POSTGRES_CA_PEM_BASE64")
		if profile == "aws-rds-postgresql" {
			rdsFile := os.Getenv("DBSPHERE_RDS_CA_FILE")
			rdsPEM := os.Getenv("DBSPHERE_RDS_CA_PEM_BASE64")
			if rdsFile != "" || rdsPEM != "" {
				file, pem64 = rdsFile, rdsPEM
			}
		}
		if file != "" {
			if pem64 != "" {
				return nil, fmt.Errorf("configure one PostgreSQL CA source")
			}
			params.Set("sslrootcert", file)
		}
	}
	address.RawQuery = params.Encode()
	config, err := pgx.ParseConfig(address.String())
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL connection settings")
	}
	if pem64 != "" {
		pem, err := base64.StdEncoding.DecodeString(pem64)
		roots := x509.NewCertPool()
		if err != nil || !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("invalid base64 PostgreSQL CA bundle")
		}
		config.TLSConfig.RootCAs = roots
	}
	config.ConnectTimeout = 4 * time.Second
	config.RuntimeParams["application_name"] = "dbsphere-monitor-go"
	config.RuntimeParams["default_transaction_read_only"] = "on"
	config.RuntimeParams["statement_timeout"] = "3000"
	return config, nil
}

// PostgreSQL only receives read-only sessions from this collector.
func PostgreSQL(ctx context.Context, target Target, password string) (map[string]any, error) {
	snapshot, err := (PostgreSQLAdapter{}).Collect(ctx, target, password)
	if err != nil {
		return nil, err
	}
	snapshot.Metrics["adapter"] = snapshot.Report
	return snapshot.Metrics, nil
}

type PostgreSQLAdapter struct{}

var _ collectoradapter.Adapter = PostgreSQLAdapter{}

func (PostgreSQLAdapter) Collect(ctx context.Context, target Target, password string) (collectoradapter.Snapshot, error) {
	config, err := readOnlyPostgresConfig(target, password)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(connectCtx, config)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	defer conn.Close(context.Background())
	var version, connections, maxConnections int
	var recovery, monitor, readStats, super, createDB, createRole, replicationRole, readOnly bool
	var commits, rollbacks, databaseBytes int64
	var serverAddress, databaseName, serverVersion string
	var serverPort int
	var startedAt time.Time
	err = conn.QueryRow(connectCtx, `SELECT current_setting('server_version_num')::int,
		pg_is_in_recovery(),pg_has_role(current_user,'pg_monitor','member'),
		pg_has_role(current_user,'pg_read_all_stats','USAGE'),
		r.rolsuper,r.rolcreatedb,r.rolcreaterole,r.rolreplication,
		coalesce(s.numbackends,0)::int,coalesce(s.xact_commit,0)::bigint,
		coalesce(s.xact_rollback,0)::bigint,pg_database_size(current_database())::bigint,
		current_setting('max_connections')::int,coalesce(host(inet_server_addr()),''),coalesce(inet_server_port(),0),
		current_database(),pg_postmaster_start_time(),current_setting('transaction_read_only')='on',pg_catalog.version()
		FROM pg_roles r LEFT JOIN pg_stat_database s ON s.datname=current_database()
		WHERE r.rolname=current_user`).Scan(&version, &recovery, &monitor, &readStats, &super, &createDB, &createRole,
		&replicationRole, &connections, &commits, &rollbacks, &databaseBytes, &maxConnections, &serverAddress, &serverPort, &databaseName, &startedAt, &readOnly, &serverVersion)
	if err != nil {
		return collectoradapter.Snapshot{}, err
	}
	major := version / 10000
	highGo459 := verifiedHighGo459(version, serverVersion)
	if !PostgresMajorSupported(major) && !highGo459 {
		return collectoradapter.Snapshot{}, fmt.Errorf("PostgreSQL %d is outside the supported %d–%d range", major, minPostgresMajor, maxPostgresMajor)
	}
	role := "primary"
	if recovery {
		role = "standby"
	}
	metrics := map[string]any{
		"version_num": version, "pg14_eol": major == 14, "role": role,
		"databaseName":      databaseName,
		"postgresEndpoint":  map[string]any{"address": serverAddress, "port": serverPort, "source": "inet_server_addr/port"},
		"postgresStartedAt": startedAt,
		"monitor_role":      monitor, "stats_visible": readStats, "privileged_role": super || createDB || createRole || replicationRole,
		"connections": connections, "max_connections": maxConnections,
		"xact_commit": commits, "xact_rollback": rollbacks, "database_bytes": databaseBytes,
		"replicas": nil, "wal_receiver": nil, "replication_lag_bytes": nil,
		"replication_observed": false,
	}
	if recovery {
		var status string
		if err := conn.QueryRow(connectCtx,
			"SELECT coalesce((SELECT status FROM pg_stat_wal_receiver LIMIT 1),'absent')").Scan(&status); err == nil {
			metrics["wal_receiver"] = status
			metrics["replication_observed"] = true
		}
	} else {
		var replicas int
		var lag *float64
		if err := conn.QueryRow(connectCtx, `SELECT count(*)::int,
			max(pg_wal_lsn_diff(pg_current_wal_lsn(),replay_lsn))::float8
			FROM pg_stat_replication`).Scan(&replicas, &lag); err == nil {
			metrics["replicas"] = replicas
			if lag != nil {
				metrics["replication_lag_bytes"] = *lag
			}
			metrics["replication_observed"] = true
		}
	}
	diagCtx, stopDiagnostics := context.WithTimeout(ctx, 15*time.Second)
	defer stopDiagnostics()
	diagnostics := collectDiagnostics(diagCtx, conn, major, recovery)
	if major >= 16 {
		var raw []byte
		err := conn.QueryRow(diagCtx, `SELECT jsonb_build_object('timing',current_setting('track_io_timing')='on',
			'rows',coalesce(jsonb_agg(to_jsonb(i)),'[]'::jsonb)) FROM pg_stat_io i`).Scan(&raw)
		if err == nil {
			metrics["pgStorageIO"] = json.RawMessage(raw)
		}
	}
	metrics["diagnostics"] = diagnostics
	sqlCtx, stopSQL := context.WithTimeout(ctx, 5*time.Second)
	defer stopSQL()
	performance := collectSQLPerformance(sqlCtx, conn, highGo459)
	metrics["sqlPerformance"] = performance
	compatibility := postgresCompatibility(major, recovery, readStats, diagnostics, performance)
	metrics["pgCompatibility"] = compatibility
	report := postgresAdapterReport(ctx, target, conn, version, role, readOnly, compatibility)
	report.Product = postgresProduct(serverVersion)
	if highGo459 {
		report.Profile = "highgo-see-4.5.9"
		waits, signal := collectHighGoWaits(ctx, conn, readStats)
		report.Signals = append(report.Signals, signal)
		if waits != nil {
			metrics["highgoWaits"] = waits
		}
	}
	return collectoradapter.Snapshot{Metrics: metrics, Report: report}, nil
}
