package collector

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

type Target struct {
	Host     string
	Port     int
	Database string
	Username string
	SSLMode  string
}

const (
	minPostgresMajor = 14
	maxPostgresMajor = 19
)

func PostgresMajorSupported(major int) bool {
	return major >= minPostgresMajor && major <= maxPostgresMajor
}

// PostgreSQL only receives read-only sessions from this collector.
func PostgreSQL(ctx context.Context, target Target, password string) (map[string]any, error) {
	mode := "disable"
	if target.SSLMode == "require" {
		mode = "verify-full"
	}
	address := url.URL{Scheme: "postgres", Host: net.JoinHostPort(target.Host, strconv.Itoa(target.Port)),
		Path: "/" + target.Database, User: url.UserPassword(target.Username, password)}
	params := url.Values{"sslmode": {mode}}
	address.RawQuery = params.Encode()
	config, err := pgx.ParseConfig(address.String())
	if err != nil {
		return nil, fmt.Errorf("invalid PostgreSQL connection settings")
	}
	config.ConnectTimeout = 4 * time.Second
	config.RuntimeParams["application_name"] = "dbsphere-monitor-go"
	config.RuntimeParams["default_transaction_read_only"] = "on"
	config.RuntimeParams["statement_timeout"] = "3000"
	connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(connectCtx, config)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	var version, connections, maxConnections int
	var recovery, monitor, readStats, super, createDB, createRole, replicationRole bool
	var commits, rollbacks, databaseBytes int64
	var serverAddress string
	var serverPort int
	err = conn.QueryRow(connectCtx, `SELECT current_setting('server_version_num')::int,
		pg_is_in_recovery(),pg_has_role(current_user,'pg_monitor','member'),
		pg_has_role(current_user,'pg_read_all_stats','USAGE'),
		r.rolsuper,r.rolcreatedb,r.rolcreaterole,r.rolreplication,
		coalesce(s.numbackends,0)::int,coalesce(s.xact_commit,0)::bigint,
		coalesce(s.xact_rollback,0)::bigint,pg_database_size(current_database())::bigint,
		current_setting('max_connections')::int,coalesce(host(inet_server_addr()),''),coalesce(inet_server_port(),0)
		FROM pg_roles r LEFT JOIN pg_stat_database s ON s.datname=current_database()
		WHERE r.rolname=current_user`).Scan(&version, &recovery, &monitor, &readStats, &super, &createDB, &createRole,
		&replicationRole, &connections, &commits, &rollbacks, &databaseBytes, &maxConnections, &serverAddress, &serverPort)
	if err != nil {
		return nil, err
	}
	major := version / 10000
	if !PostgresMajorSupported(major) {
		return nil, fmt.Errorf("PostgreSQL %d is outside the supported %d–%d range", major, minPostgresMajor, maxPostgresMajor)
	}
	role := "primary"
	if recovery {
		role = "standby"
	}
	metrics := map[string]any{
		"version_num": version, "pg14_eol": major == 14, "role": role,
		"postgresEndpoint": map[string]any{"address": serverAddress, "port": serverPort, "source": "inet_server_addr/port"},
		"monitor_role":     monitor, "privileged_role": super || createDB || createRole || replicationRole,
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
	metrics["diagnostics"] = diagnostics
	sqlCtx, stopSQL := context.WithTimeout(ctx, 5*time.Second)
	defer stopSQL()
	performance := collectSQLPerformance(sqlCtx, conn)
	metrics["sqlPerformance"] = performance
	metrics["pgCompatibility"] = postgresCompatibility(major, recovery, readStats, diagnostics, performance)
	return metrics, nil
}
