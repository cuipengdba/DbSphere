package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type httpProbe struct {
	pgURL, assetHost, assetDatabase string
	assetPort                       int
	provider                        *sdktrace.TracerProvider
}

func (p httpProbe) checkout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tracer := p.provider.Tracer("dbsphere-sdk-oom-app")
	parentCtx, parent := tracer.Start(ctx, "GET /checkout", trace.WithSpanKind(trace.SpanKindServer))
	traceID := parent.SpanContext().TraceID().String()
	dbCtx, child := tracer.Start(parentCtx, "PostgreSQL query", trace.WithSpanKind(trace.SpanKindClient))
	child.SetAttributes(attribute.String("db.system.name", "postgresql"),
		attribute.String("server.address", p.assetHost), attribute.Int("server.port", p.assetPort),
		attribute.String("db.namespace", p.assetDatabase))
	pid := 0
	conn, err := pgx.Connect(dbCtx, p.pgURL)
	if err == nil {
		err = conn.QueryRow(dbCtx, "SELECT pg_backend_pid()").Scan(&pid)
		_ = conn.Close(context.Background())
	}
	if pid > 0 {
		child.SetAttributes(attribute.Int("dbsphere.pg.backend_pid", pid))
	}
	if err != nil {
		child.SetStatus(codes.Error, "PostgreSQL connection or query failed")
		parent.SetStatus(codes.Error, "PostgreSQL connection or query failed")
	}
	child.End()
	parent.End()
	flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
	flushErr := p.provider.ForceFlush(flushCtx)
	flushCancel()
	w.Header().Set("Content-Type", "application/json")
	if err != nil || flushErr != nil {
		w.WriteHeader(http.StatusBadGateway)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	status := "ok"
	if err != nil || flushErr != nil {
		status = "error"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"traceId": traceID,
		"backendPid": pid, "status": status})
}

func serve() error {
	pgURL := required("DBSPHERE_SDK_PROBE_PG_URL")
	endpoint := required("DBSPHERE_SDK_PROBE_OTLP_ENDPOINT")
	assetHost := required("DBSPHERE_SDK_PROBE_ASSET_HOST")
	assetDatabase := required("DBSPHERE_SDK_PROBE_ASSET_DATABASE")
	port, err := strconv.Atoi(required("DBSPHERE_SDK_PROBE_ASSET_PORT"))
	if err != nil || port < 1 || port > 65535 {
		return errors.New("asset port must be between 1 and 65535")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || parsed.Path != "/v1/traces" ||
		(parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("OTLP endpoint must be an HTTP(S) /v1/traces URL")
	}
	probability, err := samplingProbability()
	if err != nil {
		return err
	}
	service := os.Getenv("DBSPHERE_SDK_PROBE_SERVICE")
	if service == "" {
		service = "dbsphere-sdk-oom-app"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	provider, err := tracerProvider(ctx, endpoint, service, probability)
	if err != nil {
		return err
	}
	defer provider.Shutdown(context.Background())
	probe := httpProbe{pgURL: pgURL, assetHost: assetHost, assetDatabase: assetDatabase,
		assetPort: port, provider: provider}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /checkout", probe.checkout)
	return (&http.Server{Addr: required("DBSPHERE_SDK_PROBE_LISTEN"), Handler: mux,
		ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 30 * time.Second}).ListenAndServe()
}
