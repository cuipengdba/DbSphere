// A standalone, real OpenTelemetry Go SDK probe for DbSphere OTLP acceptance.
// This nested module is a test utility and is not part of the DbSphere server.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"math"
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

func tracerProvider(ctx context.Context, endpoint, service string, probability *float64) (*sdktrace.TracerProvider, error) {
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, err
	}
	attributes := []attribute.KeyValue{attribute.String("service.name", service)}
	if probability != nil {
		attributes = append(attributes, attribute.Float64("dbsphere.trace.sampling_probability", *probability))
	}
	if namespace, pod, uid := os.Getenv("DBSPHERE_SDK_PROBE_K8S_NAMESPACE"),
		os.Getenv("DBSPHERE_SDK_PROBE_K8S_POD"), os.Getenv("DBSPHERE_SDK_PROBE_K8S_POD_UID"); namespace != "" && pod != "" && uid != "" {
		attributes = append(attributes, attribute.String("k8s.namespace.name", namespace),
			attribute.String("k8s.pod.name", pod), attribute.String("k8s.pod.uid", uid))
	}
	return sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter),
		sdktrace.WithResource(resource.NewWithAttributes("", attributes...))), nil
}

func samplingProbability() (*float64, error) {
	raw := os.Getenv("DBSPHERE_SDK_PROBE_SAMPLING_PROBABILITY")
	if raw == "" {
		return nil, nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value <= 0 || value > 1 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, errors.New("sampling probability must be greater than 0 and no greater than 1")
	}
	return &value, nil
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
	probability, err := samplingProbability()
	if err != nil {
		return err
	}
	provider, err := tracerProvider(ctx, endpoint, service, probability)
	if err != nil {
		return err
	}
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
	var err error
	if os.Getenv("DBSPHERE_SDK_PROBE_LISTEN") != "" {
		err = serve()
	} else {
		err = run()
	}
	if err != nil {
		log.Fatal(err)
	}
}
