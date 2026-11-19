package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/httpapi"
	"github.com/cuipengdba/dbsphere/internal/store"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// CI runs direct mode on PG14-18 and collector mode on PG18. Both exercise a
// real HTTP request and PostgreSQL lock under one DbSphere incident.
func TestSDKLockIncidentUsesOneDbSphereEventID(t *testing.T) {
	mode := os.Getenv("DBSPHERE_TEST_SDK_LOCK_MODE")
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	password := os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")
	if mode == "" || storeURL == "" || portText == "" || password == "" {
		t.Skip("SDK lock drill requires mode, PG18 store and target PostgreSQL")
	}
	if mode != "direct" && mode != "collector" {
		t.Fatal("DBSPHERE_TEST_SDK_LOCK_MODE must be direct or collector")
	}
	if mode == "collector" && runtime.GOOS != "linux" {
		t.Skip("Collector host-network drill runs on Linux CI")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		t.Fatal("invalid target PostgreSQL port")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	seed := make([]byte, 4)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(seed)
	dbName := "dbsphere_sdk_lock_" + suffix
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatal(err)
	}
	parsedStore, err := url.Parse(storeURL)
	if err != nil {
		t.Fatal(err)
	}
	parsedStore.Path = "/" + dbName
	pg, err := store.Open(ctx, parsedStore.String())
	if err != nil {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)")
		t.Fatal(err)
	}
	defer func() {
		pg.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)"); err != nil {
			t.Errorf("remove isolated SDK acceptance database: %v", err)
		}
	}()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "Official SDK lock drill", Host: "127.0.0.1",
		Port: port, Database: "postgres", Username: "postgres", Password: password,
		SSLMode: "disable", Runtime: "linux"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil || asset.ID == "" {
		t.Fatalf("create monitored asset: %v", err)
	}
	zero := 0
	if _, err := pg.UpdateAlertRule(ctx, "pg_lock_wait", store.AlertRulePatch{DurationSeconds: &zero}); err != nil {
		t.Fatal(err)
	}
	const token = "official-sdk-collector-lock-drill-write-token"
	receiver := httptest.NewServer(httpapi.New(pg, httpapi.AuthConfig{MasterKey: key, OTLPToken: token}))
	defer receiver.Close()
	var endpoint string
	if mode == "collector" {
		endpoint = startCollector(t, receiver.URL+"/v1/traces", token, suffix)
	} else {
		endpoint = startJSONRelay(t, receiver.URL+"/v1/traces", token)
	}
	probability := 1.0
	provider, err := tracerProvider(ctx, endpoint, "official-sdk-checkout", &probability)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Shutdown(context.Background())
	targetURL := (&url.URL{Scheme: "postgres", Host: "127.0.0.1:" + portText,
		Path: "/postgres", User: url.UserPassword("postgres", password),
		RawQuery: "sslmode=disable"}).String()
	blocker, err := pgx.Connect(ctx, targetURL)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(context.Background())
	lockKey := time.Now().UnixNano()
	var blockerPID int
	if err := blocker.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&blockerPID); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCtx, parent := provider.Tracer("dbsphere-sdk-lock-drill").Start(r.Context(),
			"GET /checkout", trace.WithSpanKind(trace.SpanKindServer))
		traceID := parent.SpanContext().TraceID().String()
		dbCtx, child := provider.Tracer("dbsphere-sdk-lock-drill").Start(requestCtx,
			"PostgreSQL advisory lock", trace.WithSpanKind(trace.SpanKindClient))
		conn, err := pgx.Connect(dbCtx, targetURL)
		if err != nil {
			child.End()
			parent.End()
			http.Error(w, "target connection failed", http.StatusBadGateway)
			return
		}
		defer conn.Close(context.Background())
		tx, err := conn.Begin(dbCtx)
		if err != nil {
			child.End()
			parent.End()
			http.Error(w, "target transaction failed", http.StatusBadGateway)
			return
		}
		defer tx.Rollback(context.Background())
		var pid int
		err = tx.QueryRow(dbCtx, "SELECT pg_backend_pid()").Scan(&pid)
		if err == nil {
			child.SetAttributes(attribute.String("db.system.name", "postgresql"),
				attribute.String("server.address", "127.0.0.1"), attribute.Int("server.port", port),
				attribute.String("db.namespace", "postgres"), attribute.Int("dbsphere.pg.backend_pid", pid))
			_, err = tx.Exec(dbCtx, "SELECT pg_advisory_xact_lock($1)", lockKey)
		}
		if err == nil {
			err = tx.Commit(dbCtx)
		}
		child.End()
		parent.End()
		if err == nil {
			err = provider.ForceFlush(context.Background())
		}
		if err != nil {
			http.Error(w, "target lock or trace export failed", http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"traceId": traceID, "backendPid": pid})
	}))
	defer app.Close()
	defer blocker.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
	type response struct {
		code int
		body []byte
		err  error
	}
	done := make(chan response, 1)
	go func() {
		client := http.Client{Timeout: 25 * time.Second}
		res, err := client.Get(app.URL + "/checkout")
		if err != nil {
			done <- response{err: err}
			return
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		done <- response{code: res.StatusCode, body: body, err: err}
	}()
	var waitingPID int
	for range 80 {
		err := blocker.QueryRow(ctx, `SELECT pid FROM pg_stat_activity WHERE wait_event_type='Lock'
			AND $1 = ANY(pg_blocking_pids(pid)) LIMIT 1`, blockerPID).Scan(&waitingPID)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if waitingPID == 0 {
		t.Fatal("SDK HTTP request did not wait on the real PostgreSQL lock")
	}
	checked, err := pg.CheckAsset(ctx, asset.ID, key)
	if err != nil || checked.Status != "ok" {
		t.Fatalf("DbSphere failed to sample the blocked target: %+v %v", checked, err)
	}
	value, known, err := pg.ReadCollection(ctx, "/api/events")
	if err != nil || !known {
		t.Fatal("incident list unavailable", err)
	}
	var eventID string
	for _, raw := range value.(map[string]any)["events"].([]json.RawMessage) {
		var item struct{ ID, AssetID, Status string }
		if json.Unmarshal(raw, &item) == nil && item.AssetID == asset.ID && item.Status == "open" {
			eventID = item.ID
		}
	}
	if eventID == "" {
		t.Fatal("real SDK lock did not open a DbSphere incident")
	}
	time.Sleep(650 * time.Millisecond)
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	var traceID string
	select {
	case result := <-done:
		if result.err != nil || result.code != http.StatusOK {
			t.Fatalf("SDK HTTP request failed after lock release: %d %s %v", result.code, result.body, result.err)
		}
		var body struct {
			TraceID    string `json:"traceId"`
			BackendPID int    `json:"backendPid"`
		}
		if json.Unmarshal(result.body, &body) != nil || len(body.TraceID) != 32 || body.BackendPID != waitingPID {
			t.Fatalf("SDK request PID does not match the waiting PG backend: %s", result.body)
		}
		traceID = body.TraceID
	case <-ctx.Done():
		t.Fatal("SDK HTTP request did not recover")
	}
	var incidentJSON, rawTraceJSON []byte
	for range 40 {
		incident, err := pg.EventByID(ctx, eventID)
		if err != nil {
			t.Fatal(err)
		}
		incidentJSON, _ = json.Marshal(incident)
		rawTrace, err := pg.RawTrace(ctx, asset.ID, traceID)
		if err != nil {
			t.Fatal(err)
		}
		rawTraceJSON, _ = json.Marshal(rawTrace)
		if strings.Contains(string(incidentJSON), `"code":"trace_lock_pid_matched"`) &&
			strings.Contains(string(rawTraceJSON), `"declaredSamplingProbability":1`) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, marker := range []string{eventID, `"code":"trace_lock_pid_matched"`,
		`"relation":"parent_of"`, `"relation":"same_backend_pid"`,
		`"relation":"blocked_by"`, `"declaredSpans":2`} {
		if !strings.Contains(string(incidentJSON), marker) {
			t.Fatalf("SDK/Collector lock evidence missing %s in one event: %s", marker, incidentJSON)
		}
	}
	if strings.Contains(string(incidentJSON), `"relation":"time_unreliable"`) ||
		strings.Contains(string(rawTraceJSON), password) ||
		strings.Contains(string(rawTraceJSON), "pg_advisory") {
		t.Fatalf("SDK trace has an incorrect time relation or leaks SQL/secrets: %s %s", incidentJSON, rawTraceJSON)
	}
	if report, err := pg.EventReport(ctx, eventID, "md"); err != nil ||
		!strings.Contains(report, "匹配条件：PG 等待 PID") {
		t.Fatal("same-event report lost the exact lock evidence", err)
	}
	for range 20 {
		if recovered, err := pg.CheckAsset(ctx, asset.ID, key); err != nil || recovered.Status != "ok" {
			t.Fatalf("recovery sample failed: %+v %v", recovered, err)
		}
		resolved, err := pg.EventByID(ctx, eventID)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(resolved)
		if strings.Contains(string(body), `"status":"resolved"`) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("SDK/Collector lock incident did not recover under the same event ID")
}

// The official Go OTLP/HTTP exporter emits protobuf. DbSphere intentionally
// accepts JSON only, so the direct test uses an in-process protocol adapter.
// The collector mode exercises the official adapter used by deployments.
func startJSONRelay(t *testing.T, endpoint, token string) string {
	t.Helper()
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 2<<20))
		if err != nil {
			http.Error(w, "invalid OTLP request", http.StatusBadRequest)
			return
		}
		var request collectortrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &request); err != nil {
			http.Error(w, "invalid OTLP protobuf", http.StatusBadRequest)
			return
		}
		encoded, err := protojson.Marshal(&request)
		if err != nil {
			http.Error(w, "OTLP JSON conversion failed", http.StatusBadRequest)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(encoded, &payload); err != nil {
			http.Error(w, "OTLP JSON conversion failed", http.StatusBadRequest)
			return
		}
		for _, resource := range payload["resourceSpans"].([]any) {
			for _, scope := range resource.(map[string]any)["scopeSpans"].([]any) {
				for _, value := range scope.(map[string]any)["spans"].([]any) {
					span := value.(map[string]any)
					for _, key := range []string{"traceId", "spanId", "parentSpanId"} {
						if raw, ok := span[key].(string); ok && raw != "" {
							decoded, err := base64.StdEncoding.DecodeString(raw)
							if err != nil {
								http.Error(w, "invalid OTLP span ID", http.StatusBadRequest)
								return
							}
							span[key] = hex.EncodeToString(decoded)
						}
					}
				}
			}
		}
		encoded, err = json.Marshal(payload)
		if err != nil {
			http.Error(w, "OTLP JSON conversion failed", http.StatusBadRequest)
			return
		}
		forward, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(encoded))
		if err != nil {
			http.Error(w, "OTLP forward failed", http.StatusBadGateway)
			return
		}
		forward.Header.Set("Content-Type", "application/json")
		forward.Header.Set("Authorization", "Bearer "+token)
		response, err := http.DefaultClient.Do(forward)
		if err != nil {
			http.Error(w, "OTLP forward failed", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	t.Cleanup(relay.Close)
	return relay.URL + "/v1/traces"
}

func startCollector(t *testing.T, endpoint, token, suffix string) string {
	t.Helper()
	configPath, err := filepath.Abs("../trace-lock-app/otel-collector.yaml")
	if err != nil {
		t.Fatal(err)
	}
	name := "dbsphere-sdk-lock-" + suffix
	command := exec.Command("docker", "run", "-d", "--name", name, "--network", "host",
		"-v", configPath+":/etc/otelcol/config.yaml:ro",
		"-e", "DBSPHERE_OTLP_ENDPOINT="+endpoint,
		"-e", "DBSPHERE_OTLP_TOKEN="+token,
		"otel/opentelemetry-collector:0.165.0", "--config=/etc/otelcol/config.yaml")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("start official Collector: %v: %s", err, output)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	for range 100 {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:4318", 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return "http://127.0.0.1:4318/v1/traces"
		}
		time.Sleep(100 * time.Millisecond)
	}
	logs, _ := exec.Command("docker", "logs", name).CombinedOutput()
	t.Fatalf("official Collector did not listen on 4318: %s", logs)
	return ""
}

func TestSamplingProbabilityInput(t *testing.T) {
	for _, input := range []string{"0", "1.1", "NaN", "Inf", "invalid"} {
		t.Setenv("DBSPHERE_SDK_PROBE_SAMPLING_PROBABILITY", input)
		if _, err := samplingProbability(); err == nil {
			t.Fatalf("accepted invalid sampling declaration %q", input)
		}
	}
	t.Setenv("DBSPHERE_SDK_PROBE_SAMPLING_PROBABILITY", "0.25")
	value, err := samplingProbability()
	if err != nil || value == nil || *value != 0.25 {
		t.Fatalf("valid sampling declaration was lost: %v %v", value, err)
	}
}
