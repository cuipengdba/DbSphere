package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/httpapi"
	"github.com/cuipengdba/dbsphere/internal/store"
	"github.com/jackc/pgx/v5"
)

// This drill starts a real Go HTTP application, a real Go DbSphere OTLP intake,
// PostgreSQL 14–18 as the target and an isolated PostgreSQL 18 monitoring database.
func TestApplicationLockTraceUsesOneDbSphereEventID(t *testing.T) {
	runApplicationLockTraceUsesOneDbSphereEventID(t, false, false)
}

func TestApplicationLockTraceThroughPgBouncerUsesOneDbSphereEventID(t *testing.T) {
	if os.Getenv("DBSPHERE_TEST_POOLER_URL") == "" {
		t.Skip("PgBouncer endpoint is required")
	}
	runApplicationLockTraceUsesOneDbSphereEventID(t, true, false)
}

func TestApplicationLockTraceThroughSeparatePgBouncerEndpointUsesOneDbSphereEventID(t *testing.T) {
	if os.Getenv("DBSPHERE_TEST_POOLER_URL") == "" {
		t.Skip("PgBouncer endpoint is required")
	}
	runApplicationLockTraceUsesOneDbSphereEventID(t, true, true)
}

func startPoolerTCPForwarder(t *testing.T, upstream string) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				backend, err := net.DialTimeout("tcp", upstream, 3*time.Second)
				if err != nil {
					return
				}
				defer backend.Close()
				go func() { _, _ = io.Copy(backend, client); _ = backend.(*net.TCPConn).CloseWrite() }()
				_, _ = io.Copy(client, backend)
			}()
		}
	}()
	return listener
}

func startTraceGateway(t *testing.T, target string, cfg config) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now().UTC()
		traceID, traceErr := randomHex(16)
		spanID, spanErr := randomHex(8)
		if traceErr != nil || spanErr != nil {
			http.Error(w, "trace context unavailable", http.StatusInternalServerError)
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target+"/checkout", nil)
		if err != nil {
			http.Error(w, "invalid checkout endpoint", http.StatusBadGateway)
			return
		}
		request.Header.Set("Traceparent", "00-"+traceID+"-"+spanID+"-01")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			http.Error(w, "checkout unavailable", http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		payload, err := json.Marshal(map[string]any{"resourceSpans": []any{map[string]any{
			"resource": map[string]any{"attributes": []any{attr("service.name", "dbsphere-checkout-gateway"),
				attr("dbsphere.trace.sampling_probability", "0.25")}},
			"scopeSpans": []any{map[string]any{"spans": []any{map[string]any{"traceId": traceID, "spanId": spanID,
				"kind": 2, "startTimeUnixNano": strconv.FormatInt(started.UnixNano(), 10),
				"endTimeUnixNano": strconv.FormatInt(time.Now().UnixNano(), 10)}}}},
		}}})
		if err != nil || exportTrace(cfg, http.DefaultClient, payload) != nil {
			http.Error(w, "gateway trace unavailable", http.StatusBadGateway)
			return
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
}

func runApplicationLockTraceUsesOneDbSphereEventID(t *testing.T, viaPooler, separatePoolerEndpoint bool) {
	t.Helper()
	storeURL := os.Getenv("DBSPHERE_TEST_PG18_URL")
	portText := os.Getenv("DBSPHERE_TEST_PG_TARGET_PORT")
	password := os.Getenv("DBSPHERE_TEST_PG_TARGET_PASSWORD")
	if storeURL == "" || portText == "" || password == "" {
		t.Skip("PG18 monitoring store and PostgreSQL target are required")
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	targetHost := os.Getenv("DBSPHERE_TEST_PG_TARGET_HOST")
	if targetHost == "" {
		targetHost = "127.0.0.1"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, storeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	suffix, err := randomHex(4)
	if err != nil {
		t.Fatal(err)
	}
	dbName := "dbsphere_trace_" + suffix
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
			t.Errorf("remove isolated monitoring database: %v", err)
		}
	}()
	key := []byte("01234567890123456789012345678901")
	assetInput := assets.Input{Name: "Real HTTP lock drill", Host: targetHost,
		Port: port, Database: "postgres", Username: "postgres", Password: password,
		SSLMode: "disable", Runtime: "linux"}
	if viaPooler {
		assetInput.PoolerURL = os.Getenv("DBSPHERE_TEST_POOLER_URL")
		if separatePoolerEndpoint {
			management, err := url.Parse(assetInput.PoolerURL)
			if err != nil {
				t.Fatal(err)
			}
			assetInput.PoolerEndpoint = startPoolerTCPForwarder(t, management.Host).Addr().String()
		}
	}
	created, err := pg.CreateAsset(ctx, assetInput, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	zero := 0
	if _, err := pg.UpdateAlertRule(ctx, "pg_lock_wait", store.AlertRulePatch{DurationSeconds: &zero}); err != nil {
		t.Fatal(err)
	}
	const token = "real-app-go-otlp-token-at-least-32-characters"
	receiver := httptest.NewServer(httpapi.New(pg, httpapi.AuthConfig{MasterKey: key,
		OTLPToken: token}))
	defer receiver.Close()
	targetURL := (&url.URL{Scheme: "postgres", Host: net.JoinHostPort(targetHost, portText),
		Path: "/postgres", User: url.UserPassword("postgres", password),
		RawQuery: "sslmode=disable"}).String()
	appURL := targetURL
	poolerSecret := ""
	if viaPooler {
		parsedPooler, err := url.Parse(assetInput.PoolerURL)
		if err != nil {
			t.Fatal(err)
		}
		poolerSecret, _ = parsedPooler.User.Password()
		parsedPooler.Path = "/postgres"
		if separatePoolerEndpoint {
			parsedPooler.Host = assetInput.PoolerEndpoint
		}
		appURL = parsedPooler.String()
	}
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
	appConfig := config{pgURL: appURL, endpoint: receiver.URL + "/v1/traces", token: token, lockKey: lockKey}
	app := httptest.NewServer(newHandler(appConfig,
		&http.Client{Timeout: 5 * time.Second}))
	defer app.Close()
	entryPoint := app.URL
	if viaPooler {
		gateway := startTraceGateway(t, app.URL, appConfig)
		defer gateway.Close()
		entryPoint = gateway.URL
	}
	defer blocker.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
	type response struct {
		code int
		body []byte
		err  error
	}
	done := make(chan response, 1)
	traceID := ""
	go func() {
		res, err := http.Get(entryPoint + "/checkout")
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
		t.Fatal("HTTP request did not wait on the real PostgreSQL lock")
	}
	checked, err := pg.CheckAsset(ctx, asset.ID, key)
	if err != nil || checked.Status != "ok" {
		t.Fatalf("DbSphere did not collect the blocked target: %+v %v", checked, err)
	}
	if separatePoolerEndpoint {
		rawSample, err := pg.RawSample(ctx, asset.ID, checked.SampleID)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(rawSample)
		var evidence struct {
			Sample struct {
				Metrics struct {
					PgBouncer struct {
						Status string `json:"status"`
					} `json:"pgbouncer"`
				} `json:"metrics"`
			} `json:"sample"`
		}
		if json.Unmarshal(body, &evidence) != nil || evidence.Sample.Metrics.PgBouncer.Status != "ok" {
			t.Fatalf("separate management URL did not collect PgBouncer statistics: %s", body)
		}
	}
	value, known, err := pg.ReadCollection(ctx, "/api/events")
	if err != nil || !known {
		t.Fatal("DbSphere incident list unavailable", err)
	}
	var eventID string
	for _, raw := range value.(map[string]any)["events"].([]json.RawMessage) {
		var item struct {
			ID, AssetID, Status string
		}
		if json.Unmarshal(raw, &item) == nil && item.AssetID == asset.ID && item.Status == "open" {
			eventID = item.ID
		}
	}
	if eventID == "" {
		t.Fatal("the real lock alert did not open one DbSphere incident")
	}
	// The RCA rule intentionally requires a slow database call (at least 500 ms).
	time.Sleep(650 * time.Millisecond)
	if _, err := blocker.Exec(ctx, "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.code != http.StatusOK {
			t.Fatalf("HTTP request failed after lock release: %d %s %v", got.code, got.body, got.err)
		}
		var body struct {
			BackendPID int    `json:"backendPid"`
			TraceID    string `json:"traceId"`
		}
		if json.Unmarshal(got.body, &body) != nil || body.BackendPID != waitingPID || len(body.TraceID) != 32 {
			t.Fatalf("request PID did not match the locked backend: %s", got.body)
		}
		traceID = body.TraceID
	case <-ctx.Done():
		t.Fatal("HTTP request did not recover")
	}
	detail, err := pg.EventByID(ctx, eventID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(detail)
	for _, marker := range []string{eventID, `"code":"trace_lock_pid_matched"`,
		`"relation":"parent_of"`, `"relation":"same_backend_pid"`,
		`"relation":"blocked_by"`} {
		if !strings.Contains(string(encoded), marker) {
			t.Fatalf("real application lock evidence missing %s in one incident: %s", marker, encoded)
		}
	}
	if viaPooler {
		for _, marker := range []string{`"relation":"via_pooler"`, `"relation":"forwards_to"`, `"relation":"calls_service"`,
			`"type":"pgbouncer"`, `"parentState":"complete"`, `"service":"dbsphere-checkout-gateway"`,
			`"samplingProbability":0.25`, `"backendState":"matched"`, `"poolerState":"pool_sample_observed"`,
			`"backendSampleId":"` + checked.SampleID + `"`} {
			if !strings.Contains(string(encoded), marker) {
				t.Fatalf("real PgBouncer lock chain is missing %s: %s", marker, encoded)
			}
		}
	}
	rawTrace, err := pg.RawTrace(ctx, asset.ID, traceID)
	if err != nil || rawTrace == nil {
		t.Fatal("the application trace could not be opened from the same asset", err)
	}
	rawTraceJSON, _ := json.Marshal(rawTrace)
	wantRoute := `"route":"direct"`
	if viaPooler {
		wantRoute = `"route":"pgbouncer"`
	}
	if !strings.Contains(string(rawTraceJSON), wantRoute) ||
		strings.Contains(string(rawTraceJSON), "pg_advisory") ||
		strings.Contains(string(rawTraceJSON), password) ||
		(poolerSecret != "" && strings.Contains(string(rawTraceJSON), poolerSecret)) {
		t.Fatalf("real application trace has an incorrect route or leaked a secret: %s", rawTraceJSON)
	}
	if report, err := pg.EventReport(ctx, eventID, "md"); err != nil ||
		!strings.Contains(report, "匹配条件：PG 等待 PID") {
		t.Fatal("incident report lost the exact lock evidence", err)
	}
	lastStatus := ""
	for range 20 {
		if recovered, err := pg.CheckAsset(ctx, asset.ID, key); err != nil || recovered.Status != "ok" {
			t.Fatalf("recovery sample failed: %+v %v", recovered, err)
		}
		resolved, err := pg.EventByID(ctx, eventID)
		if err != nil || resolved == nil {
			t.Fatal("original event disappeared during recovery", err)
		}
		var incident struct {
			Event struct {
				Status string `json:"status"`
			} `json:"event"`
		}
		body, _ := json.Marshal(resolved)
		if err := json.Unmarshal(body, &incident); err != nil {
			t.Fatal(err)
		}
		lastStatus = incident.Event.Status
		if lastStatus == "resolved" {
			if separatePoolerEndpoint {
				if proof := os.Getenv("DBSPHERE_TRACE_ACCEPTANCE_PROOF"); proof != "" {
					combined := map[string]any{"event": resolved, "trace": rawTrace, "traceId": traceID,
						"backendPid": waitingPID, "blockerPid": blockerPID, "sampleId": checked.SampleID}
					payload, err := json.MarshalIndent(combined, "", "  ")
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(proof, payload, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			t.Logf("event %s, trace %s, locked backend %d, blocker %d, source %s, through PgBouncer %t", eventID,
				traceID, waitingPID, blockerPID, checked.SampleID, viaPooler)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("recovery did not close the same incident ID; last status %s", lastStatus)
}
