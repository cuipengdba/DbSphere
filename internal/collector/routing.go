package collector

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type RouteProbe struct {
	Status     string    `json:"status"`
	Backend    string    `json:"backend,omitempty"`
	Database   string    `json:"database,omitempty"`
	Role       string    `json:"role,omitempty"`
	StartedAt  time.Time `json:"-"`
	ObservedAt time.Time `json:"observedAt"`
	DurationMS int64     `json:"durationMs"`
	SQLState   string    `json:"sqlState,omitempty"`
	Source     string    `json:"source"`
}

// ProbePostgreSQLRoute opens one bounded, read-only session through the business endpoint.
func ProbePostgreSQLRoute(ctx context.Context, target Target, password string) (probe RouteProbe) {
	start := time.Now()
	probe.Source = "PostgreSQL read-only transaction through configured entry"
	defer func() {
		probe.ObservedAt = time.Now().UTC()
		probe.DurationMS = time.Since(start).Milliseconds()
	}()
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	cfg, err := readOnlyPostgresConfig(target, password)
	if err != nil {
		probe.Status = "configuration_error"
		return
	}
	// Transaction poolers need these settings at transaction scope.
	delete(cfg.RuntimeParams, "default_transaction_read_only")
	delete(cfg.RuntimeParams, "statement_timeout")
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		probe.Status, probe.SQLState = routeProbeError(err)
		return
	}
	defer conn.Close(ctx)
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		probe.Status, probe.SQLState = routeProbeError(err)
		return
	}
	defer tx.Rollback(ctx)
	var recovery bool
	var host string
	var port int
	err = tx.QueryRow(ctx, `SELECT pg_is_in_recovery(),coalesce(host(inet_server_addr()),''),
		coalesce(inet_server_port(),0),current_database(),pg_postmaster_start_time()`).
		Scan(&recovery, &host, &port, &probe.Database, &probe.StartedAt)
	if err != nil {
		probe.Status, probe.SQLState = routeProbeError(err)
		return
	}
	probe.Status, probe.Role = "ok", "primary"
	if recovery {
		probe.Role = "standby"
	}
	if host != "" && port > 0 {
		probe.Backend = net.JoinHostPort(host, strconv.Itoa(port))
	}
	return
}

func routeProbeError(err error) (string, string) {
	if errors.Is(err, context.Canceled) {
		return "cancelled", ""
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return "postgres_rejected", pgErr.Code
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_error", ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", ""
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) && netErr.Op == "dial" {
		return "unreachable", ""
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return "timeout", ""
	}
	return "connection_error", ""
}

// HAProxyStatsURL accepts a single configured stats origin.
func HAProxyStatsURL(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || len(value) > 2048 || u.Hostname() == "" || u.Fragment != "" ||
		(u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("invalid HAProxy statistics URL")
	}
	return u, nil
}

func HAProxyStats(ctx context.Context, endpoint, backend string) map[string]any {
	result := map[string]any{"status": "unavailable", "backend": backend, "source": "HAProxy HTTP CSV"}
	defer func() { result["observedAt"] = time.Now().UTC() }()
	u, err := HAProxyStatsURL(endpoint)
	if err != nil {
		return result
	}
	if !strings.HasSuffix(u.Path, ";csv") {
		u.Path += ";csv"
		u.RawPath = ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return result
	}
	client := http.Client{Timeout: 4 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return result
	}
	defer resp.Body.Close()
	result["httpStatus"] = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		return result
	}
	bounded := &io.LimitedReader{R: resp.Body, N: (2 << 20) + 1}
	r := csv.NewReader(bounded)
	r.FieldsPerRecord = -1
	header, err := r.Read()
	if err != nil || !strings.HasPrefix(header[0], "#") {
		return result
	}
	cols := make(map[string]int, len(header))
	for i, name := range header {
		cols[strings.TrimSpace(strings.TrimPrefix(name, "#"))] = i
	}
	for _, name := range []string{"pxname", "svname", "status", "qcur", "qmax"} {
		if _, exists := cols[name]; !exists {
			return result
		}
	}
	rows := []map[string]any{}
	truncated := false
	for {
		row, err := r.Read()
		if errors.Is(err, io.EOF) && bounded.N > 0 {
			break
		}
		if err != nil || len(row) != len(header) {
			return result
		}
		if row[cols["pxname"]] != backend || row[cols["svname"]] == "FRONTEND" {
			continue
		}
		if len(rows) == 64 {
			truncated = true
			break
		}
		fields := map[string]any{"name": row[cols["svname"]], "status": row[cols["status"]]}
		for _, name := range []string{"addr", "check_status"} {
			if i, exists := cols[name]; exists {
				fields[name] = row[i]
			}
		}
		for _, name := range []string{"qcur", "qmax", "scur", "slim"} {
			if i, exists := cols[name]; exists {
				if value, err := strconv.Atoi(row[i]); err == nil && value >= 0 {
					fields[name] = value
				}
			}
		}
		rows = append(rows, fields)
	}
	if len(rows) == 0 {
		result["status"] = "backend_missing"
		return result
	}
	result["status"], result["rows"], result["truncated"] = "ok", rows, truncated
	return result
}
