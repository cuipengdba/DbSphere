// A standalone, real OpenTelemetry Go SDK probe for DbSphere OTLP acceptance.
// This nested module is a test utility and is not part of the DbSphere server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func required(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatal(name + " is required")
	}
	return value
}

func run() error {
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
	service := os.Getenv("DBSPHERE_SDK_PROBE_SERVICE")
	if service == "" {
		service = "dbsphere-sdk-probe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return err
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithResource(
		resource.NewWithAttributes("", attribute.String("service.name", service))))
	defer provider.Shutdown(context.Background())
	tracer := provider.Tracer("dbsphere-sdk-probe")
	requestCtx, parent := tracer.Start(ctx, "checkout", trace.WithSpanKind(trace.SpanKindServer))
	traceID := parent.SpanContext().TraceID().String()
	dbCtx, child := tracer.Start(requestCtx, "postgresql query", trace.WithSpanKind(trace.SpanKindClient))
	conn, err := pgx.Connect(dbCtx, pgURL)
	if err != nil {
		child.End()
		parent.End()
		return err
	}
	var pid int
	if err = conn.QueryRow(dbCtx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		_ = conn.Close(dbCtx)
		child.End()
		parent.End()
		return err
	}
	_ = conn.Close(dbCtx)
	child.SetAttributes(attribute.String("db.system.name", "postgresql"),
		attribute.String("server.address", assetHost), attribute.Int("server.port", port),
		attribute.String("db.namespace", assetDatabase), attribute.Int("dbsphere.pg.backend_pid", pid))
	child.End()
	parent.End()
	if err := provider.ForceFlush(context.Background()); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"traceId": traceID, "backendPid": pid})
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
