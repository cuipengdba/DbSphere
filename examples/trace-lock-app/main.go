// A small PostgreSQL HTTP service for DbSphere's application-to-lock acceptance drill.
// It emits bounded OTLP/HTTP JSON spans without adding a runtime dependency.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type config struct {
	pgURL             string
	endpoint          string
	token             string
	listen            string
	lockKey           int64
	measureConnection bool
	pool              *pgxpool.Pool
	acquireTimeout    time.Duration
}

type phaseTiming struct {
	kind, spanID string
	start, end   time.Time
	err          error
}

type demoTraceContext struct {
	traceID, parentSpanID string
}

func demoTraceParent(header string) (demoTraceContext, error) {
	fields := strings.Split(header, "-")
	if len(fields) == 4 && fields[0] == "00" && len(fields[1]) == 32 && len(fields[2]) == 16 && len(fields[3]) == 2 {
		_, traceErr := hex.DecodeString(fields[1])
		_, spanErr := hex.DecodeString(fields[2])
		_, flagsErr := hex.DecodeString(fields[3])
		if traceErr == nil && spanErr == nil && flagsErr == nil && strings.Trim(fields[1], "0") != "" && strings.Trim(fields[2], "0") != "" {
			return demoTraceContext{traceID: strings.ToLower(fields[1]), parentSpanID: strings.ToLower(fields[2])}, nil
		}
	}
	id, err := randomHex(16)
	return demoTraceContext{traceID: id}, err
}

func readConfig() (config, error) {
	cfg := config{pgURL: os.Getenv("DBSPHERE_DEMO_PG_URL"),
		endpoint: os.Getenv("DBSPHERE_DEMO_OTLP_ENDPOINT"),
		token:    os.Getenv("DBSPHERE_DEMO_OTLP_TOKEN"), listen: os.Getenv("DBSPHERE_DEMO_LISTEN"),
		lockKey: 1742026, measureConnection: os.Getenv("DBSPHERE_DEMO_CONNECTION_STAGES") == "true",
		acquireTimeout: 500 * time.Millisecond}
	if cfg.listen == "" {
		cfg.listen = "127.0.0.1:18092"
	}
	if value := os.Getenv("DBSPHERE_DEMO_LOCK_KEY"); value != "" {
		key, err := strconv.ParseInt(value, 10, 64)
		if err != nil || key <= 0 {
			return cfg, errors.New("DBSPHERE_DEMO_LOCK_KEY must be a positive integer")
		}
		cfg.lockKey = key
	}
	endpoint, err := url.Parse(cfg.endpoint)
	if cfg.pgURL == "" || err != nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" ||
		endpoint.Host == "" || endpoint.Path != "/v1/traces" || len(cfg.token) < 32 {
		return cfg, errors.New("set PG URL, /v1/traces OTLP endpoint and a 32-character token")
	}
	return cfg, nil
}

func randomHex(bytesCount int) (string, error) {
	buf := make([]byte, bytesCount)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func attr(key, value string) map[string]any {
	return map[string]any{"key": key, "value": map[string]string{"stringValue": value}}
}

func traceBody(parentContext demoTraceContext, serverID, dbID, host, database string, port, pid int,
	serverStart, dbStart, dbEnd, serverEnd time.Time, queryErr error, phases ...phaseTiming) ([]byte, error) {
	status := 1
	if queryErr != nil {
		status = 2
	}
	parent := map[string]any{"traceId": parentContext.traceID, "spanId": serverID, "kind": 2,
		"startTimeUnixNano": strconv.FormatInt(serverStart.UnixNano(), 10),
		"endTimeUnixNano":   strconv.FormatInt(serverEnd.UnixNano(), 10),
		"status":            map[string]any{"code": status}}
	if parentContext.parentSpanID != "" {
		parent["parentSpanId"] = parentContext.parentSpanID
	}
	clientSpan := func(id string, start, end time.Time, code int, phaseAttrs ...any) map[string]any {
		identity := []any{attr("db.system.name", "postgresql"), attr("server.address", host),
			attr("server.port", strconv.Itoa(port)), attr("db.namespace", database), attr("dbsphere.endpoint", "checkout")}
		return map[string]any{"traceId": parentContext.traceID, "spanId": id, "parentSpanId": serverID,
			"kind": 3, "startTimeUnixNano": strconv.FormatInt(start.UnixNano(), 10),
			"endTimeUnixNano": strconv.FormatInt(end.UnixNano(), 10),
			"status":          map[string]any{"code": code}, "attributes": append(identity, phaseAttrs...)}
	}
	spans := []any{parent}
	if !dbStart.IsZero() {
		spans = append(spans, clientSpan(dbID, dbStart, dbEnd, status, attr("dbsphere.pg.backend_pid", strconv.Itoa(pid))))
	}
	for _, phase := range phases {
		code := 1
		attrs := []any{attr("dbsphere.connection.phase", phase.kind)}
		if phase.err != nil {
			code = 2
			var timeout interface{ Timeout() bool }
			if errors.Is(phase.err, context.DeadlineExceeded) || (errors.As(phase.err, &timeout) && timeout.Timeout()) {
				attrs = append(attrs, attr("dbsphere.connection.outcome", "timeout"))
			}
		}
		spans = append(spans, clientSpan(phase.spanID, phase.start, phase.end, code, attrs...))
	}
	return json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
		"resource":   map[string]any{"attributes": []any{attr("service.name", "dbsphere-lock-demo")}},
		"scopeSpans": []any{map[string]any{"spans": spans}}}}})
}

func runCheckout(ctx context.Context, cfg config, client *http.Client, traceparent string) (string, int, error) {
	serverStart := time.Now().UTC()
	parent, err := demoTraceParent(traceparent)
	if err != nil {
		return "", 0, err
	}
	serverID, err := randomHex(8)
	if err != nil {
		return "", 0, err
	}
	dbID, err := randomHex(8)
	if err != nil {
		return "", 0, err
	}
	parsed, err := pgx.ParseConfig(cfg.pgURL)
	if err != nil {
		return "", 0, err
	}
	connectionStart := time.Now().UTC()
	var conn *pgx.Conn
	var release func()
	connectionKind := "connect"
	if cfg.pool != nil {
		connectionKind = "pool_acquire"
		acquireCtx, cancel := context.WithTimeout(ctx, cfg.acquireTimeout)
		var pooled *pgxpool.Conn
		pooled, err = cfg.pool.Acquire(acquireCtx)
		cancel()
		if err == nil {
			conn, release = pooled.Conn(), pooled.Release
		}
	} else {
		conn, err = pgx.Connect(ctx, cfg.pgURL)
		if err == nil {
			release = func() { _ = conn.Close(context.Background()) }
		}
	}
	if release != nil {
		defer release()
	}
	dbStart := connectionStart
	var phases []phaseTiming
	if cfg.measureConnection {
		acquiredAt := time.Now().UTC()
		phaseID, randomErr := randomHex(8)
		if randomErr != nil {
			return parent.traceID, 0, randomErr
		}
		phases = append(phases, phaseTiming{kind: connectionKind, spanID: phaseID, start: connectionStart, end: acquiredAt, err: err})
		dbStart = time.Time{}
		if err == nil {
			dbStart = acquiredAt
		}
	}
	pid := 0
	if err == nil {
		var tx pgx.Tx
		tx, err = conn.Begin(ctx)
		if err == nil {
			defer tx.Rollback(context.Background())
			err = tx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
			if err == nil {
				_, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1)", cfg.lockKey)
			}
			if err == nil {
				err = tx.Commit(ctx)
			}
		}
	}
	dbEnd := time.Now().UTC()
	serverEnd := dbEnd
	body, marshalErr := traceBody(parent, serverID, dbID, parsed.Host, parsed.Database,
		int(parsed.Port), pid, serverStart, dbStart, dbEnd, serverEnd, err, phases...)
	if marshalErr != nil {
		return parent.traceID, pid, marshalErr
	}
	if exportErr := exportTrace(cfg, client, body); exportErr != nil {
		return parent.traceID, pid, exportErr
	}
	return parent.traceID, pid, err
}

func exportTrace(cfg config, client *http.Client, body []byte) error {
	exportCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, requestErr := http.NewRequestWithContext(exportCtx, http.MethodPost, cfg.endpoint,
		bytes.NewReader(body))
	if requestErr != nil {
		return requestErr
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+cfg.token)
	response, exportErr := client.Do(request)
	if exportErr != nil {
		return exportErr
	}
	defer response.Body.Close()
	if response.StatusCode/100 != 2 {
		return fmt.Errorf("OTLP receiver returned HTTP %d", response.StatusCode)
	}
	return nil
}

func newHandler(cfg config, client *http.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/checkout", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		traceID, pid, err := runCheckout(ctx, cfg, client, r.Header.Get("Traceparent"))
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			log.Printf("checkout failed: %v", err)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"traceId": traceID, "backendPid": pid,
			"status": map[bool]string{true: "error", false: "ok"}[err != nil]})
	})
	return mux
}

func main() {
	cfg, err := readConfig()
	if err != nil {
		log.Fatal(err)
	}
	if value := os.Getenv("DBSPHERE_DEMO_POOL_SIZE"); value != "" {
		size, err := strconv.ParseInt(value, 10, 32)
		if err != nil || size < 1 || size > 32 {
			log.Fatal("DBSPHERE_DEMO_POOL_SIZE must be between 1 and 32")
		}
		poolConfig, err := pgxpool.ParseConfig(cfg.pgURL)
		if err != nil {
			log.Fatal("invalid PostgreSQL pool URL")
		}
		poolConfig.MaxConns = int32(size)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		cfg.pool, err = pgxpool.NewWithConfig(ctx, poolConfig)
		if err == nil {
			err = cfg.pool.Ping(ctx)
		}
		cancel()
		if err != nil {
			log.Fatal("unable to warm example PostgreSQL pool")
		}
		defer cfg.pool.Close()
		cfg.measureConnection = true
	}
	client := &http.Client{Timeout: 5 * time.Second}
	log.Printf("DbSphere lock demo listening on %s", cfg.listen)
	log.Fatal(http.ListenAndServe(cfg.listen, newHandler(cfg, client)))
}
