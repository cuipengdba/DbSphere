package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/cuipengdba/dbsphere/internal/store"
	"github.com/cuipengdba/dbsphere/internal/trace"
)

type healthStub struct{ err error }

func (stub healthStub) Health(context.Context) error { return stub.err }

type traceStub struct {
	healthStub
	spans []trace.Span
}

func (*traceStub) TraceAssets(context.Context) ([]trace.Asset, error) {
	return []trace.Asset{{ID: "11111111-1111-1111-1111-111111111111",
		Host: "pg.internal", Port: 5432, Database: "orders"}}, nil
}

func (stub *traceStub) SaveTraceSpans(_ context.Context, spans []trace.Span) (int, error) {
	stub.spans = append(stub.spans, spans...)
	return len(spans), nil
}

func TestGoOTLPIntakeRequiresSeparateTokenAndBounds(t *testing.T) {
	token := strings.Repeat("t", 32)
	stub := &traceStub{}
	call := func(handler http.Handler, auth, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := call(New(stub, AuthConfig{}), "Bearer "+token, `{}`); got.Code != http.StatusNotFound {
		t.Fatal("disabled trace intake accepted data")
	}
	handler := New(stub, AuthConfig{OTLPToken: token})
	if got := call(handler, token, `{}`); got.Code != http.StatusUnauthorized {
		t.Fatal("trace token without Bearer accepted")
	}
	if got := call(handler, "Bearer wrong", `{}`); got.Code != http.StatusUnauthorized {
		t.Fatal("wrong trace token accepted")
	}
	if got := call(handler, "Bearer "+token, strings.Repeat("x", 1<<20+1)); got.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized trace body accepted: %d", got.Code)
	}
	now := time.Now().UnixNano()
	payload := fmt.Sprintf(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"checkout"}}]},"scopeSpans":[{"spans":[{"traceId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spanId":"1111111111111111","kind":3,"startTimeUnixNano":"%d","endTimeUnixNano":"%d","attributes":[{"key":"db.system.name","value":{"stringValue":"postgresql"}},{"key":"server.address","value":{"stringValue":"pg.internal"}},{"key":"db.namespace","value":{"stringValue":"orders"}}]}]}]}]}`, now-1000000, now)
	if got := call(handler, "Bearer "+token, payload); got.Code != http.StatusOK || len(stub.spans) != 1 ||
		stub.spans[0].AssetID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("valid OTLP span was not mapped: %d %s", got.Code, got.Body.String())
	}
}

type silenceStub struct {
	healthStub
	created, revoked int
}

func (stub *silenceStub) RetryNotification(_ context.Context, id string) (bool, error) {
	return id == "11111111-1111-1111-1111-111111111111", nil
}

func (stub *silenceStub) CreateSilence(_ context.Context, input store.SilenceInput) (json.RawMessage, error) {
	stub.created++
	if input.Reason == "invalid" {
		return nil, store.ErrInvalidSilence
	}
	return json.RawMessage(`{"id":"11111111-1111-1111-1111-111111111111"}`), nil
}

func (stub *silenceStub) RevokeSilence(_ context.Context, id string) (json.RawMessage, error) {
	stub.revoked++
	if id == "22222222-2222-2222-2222-222222222222" {
		return nil, store.ErrSilenceNotFound
	}
	return json.RawMessage(`{"id":"` + id + `","revokedAt":"2026-11-17T00:00:00Z"}`), nil
}

func TestGoSilenceAPIRequiresLoginAndCSRF(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	hash, err := security.HashPassword("silence-test-password")
	if err != nil {
		t.Fatal(err)
	}
	stub := &silenceStub{}
	handler := New(stub, AuthConfig{MasterKey: key, PasswordHash: hash})
	call := func(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if csrf != "" {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := call(http.MethodPost, "/api/silences", `{}`, nil, ""); got.Code != http.StatusUnauthorized {
		t.Fatal("anonymous silence accepted")
	}
	login := call(http.MethodPost, "/api/login", `{"password":"silence-test-password"}`, nil, "")
	if login.Code != http.StatusOK {
		t.Fatalf("login: %d", login.Code)
	}
	cookie := login.Result().Cookies()[0]
	if got := call(http.MethodPost, "/api/silences", `{}`, cookie, ""); got.Code != http.StatusForbidden {
		t.Fatal("missing CSRF accepted")
	}
	csrf := security.CSRFToken(key, cookie.Value)
	if got := call(http.MethodPost, "/api/silences", `{"reason":"valid"}`, cookie, csrf); got.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodPost, "/api/silences", `{"reason":"invalid"}`, cookie, csrf); got.Code != http.StatusBadRequest {
		t.Fatal("invalid silence accepted")
	}
	if got := call(http.MethodDelete, "/api/silences/11111111-1111-1111-1111-111111111111", "", cookie, csrf); got.Code != http.StatusOK {
		t.Fatalf("revoke: %d", got.Code)
	}
	if got := call(http.MethodDelete, "/api/silences/22222222-2222-2222-2222-222222222222", "", cookie, csrf); got.Code != http.StatusNotFound {
		t.Fatal("missing silence not reported")
	}
	if got := call(http.MethodPost, "/api/notifications/11111111-1111-1111-1111-111111111111/retry", "", cookie, csrf); got.Code != http.StatusOK {
		t.Fatalf("notification retry failed: %d", got.Code)
	}
	if got := call(http.MethodPost, "/api/notifications/22222222-2222-2222-2222-222222222222/retry", "", cookie, csrf); got.Code != http.StatusNotFound {
		t.Fatal("missing failed notification not reported")
	}
	if stub.created != 2 || stub.revoked != 2 {
		t.Fatalf("wrong calls: %d %d", stub.created, stub.revoked)
	}
}

type alertRuleStub struct {
	healthStub
	called int
}

func (stub *alertRuleStub) UpdateAlertRule(_ context.Context, id string, patch store.AlertRulePatch) (any, error) {
	stub.called++
	if id == "missing_rule" {
		return nil, store.ErrAlertRuleNotFound
	}
	return map[string]any{"rule": map[string]any{"id": id, "enabled": patch.Enabled}}, nil
}

func TestGoAlertRulePatchRequiresLoginAndCSRF(t *testing.T) {
	hash, err := security.HashPassword("rule-test-password")
	if err != nil {
		t.Fatal(err)
	}
	stub := &alertRuleStub{}
	handler := New(stub, AuthConfig{MasterKey: []byte("01234567890123456789012345678901"), PasswordHash: hash})
	patch := func(cookie *http.Cookie, csrf string, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		if cookie != nil {
			request.AddCookie(cookie)
		}
		if csrf != "" {
			request.Header.Set("X-CSRF-Token", csrf)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	if response := patch(nil, "", "/api/alert-rules/pg_connections_high", `{"enabled":false}`); response.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous rule update: %d", response.Code)
	}
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"password":"rule-test-password"}`))
	loginRequest.Header.Set("Content-Type", "application/json")
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("rule test login failed: %d", login.Code)
	}
	cookie := login.Result().Cookies()[0]
	if response := patch(cookie, "", "/api/alert-rules/pg_connections_high", `{"enabled":false}`); response.Code != http.StatusForbidden {
		t.Fatalf("rule update without CSRF: %d", response.Code)
	}
	csrf := security.CSRFToken([]byte("01234567890123456789012345678901"), cookie.Value)
	if response := patch(cookie, csrf, "/api/alert-rules/pg_connections_high", `{"enabled":false}`); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "pg_connections_high") {
		t.Fatalf("valid rule update failed: %d %s", response.Code, response.Body.String())
	}
	if response := patch(cookie, csrf, "/api/alert-rules/pg_connections_high", `{"threshold":"wrong"}`); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid rule update accepted: %d", response.Code)
	}
	if response := patch(cookie, csrf, "/api/alert-rules/missing_rule", `{"enabled":true}`); response.Code != http.StatusNotFound {
		t.Fatalf("missing rule status: %d", response.Code)
	}
	if stub.called != 2 {
		t.Fatalf("unexpected storage calls: %d", stub.called)
	}
}

func TestEmbeddedFrontendAndReadiness(t *testing.T) {
	handler := New(healthStub{})
	for _, path := range []string{"/", "/app.js", "/styles.css", "/favicon.svg"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || response.Body.Len() == 0 {
			t.Fatalf("embedded %s unavailable: %d", path, response.Code)
		}
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "postgresql18") {
		t.Fatalf("PG18 readiness missing: %d %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	New(healthStub{err: errors.New("storage down")}).ServeHTTP(response,
		httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("storage failure should fail readiness: %d", response.Code)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/private/file", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown static path exposed: %d", response.Code)
	}
}

func TestGoLoginSessionAndCSRF(t *testing.T) {
	hash, err := security.HashPassword("migration-password")
	if err != nil {
		t.Fatal(err)
	}
	handler := New(healthStub{}, AuthConfig{MasterKey: []byte("01234567890123456789012345678901"), PasswordHash: hash})
	request := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"password":"migration-password"}`))
	request.Header.Set("Content-Type", "application/json")
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, request)
	if login.Code != http.StatusOK || len(login.Result().Cookies()) != 1 ||
		!strings.Contains(login.Body.String(), `"authenticated":true`) {
		t.Fatalf("Go login failed: %d %s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("session cookie security flags missing")
	}
	sessionRequest := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	sessionRequest.AddCookie(cookie)
	session := httptest.NewRecorder()
	handler.ServeHTTP(session, sessionRequest)
	if session.Code != http.StatusOK || !strings.Contains(session.Body.String(), `"authenticated":true`) {
		t.Fatalf("Go session missing: %d %s", session.Code, session.Body.String())
	}
	logoutRequest := httptest.NewRequest(http.MethodPost, "/api/logout", nil)
	logoutRequest.AddCookie(cookie)
	missingCSRF := httptest.NewRecorder()
	handler.ServeHTTP(missingCSRF, logoutRequest)
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatal("missing CSRF accepted")
	}
	logoutRequest.Header.Set("X-CSRF-Token", security.CSRFToken([]byte("01234567890123456789012345678901"), cookie.Value))
	logout := httptest.NewRecorder()
	handler.ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusOK {
		t.Fatalf("logout failed: %d", logout.Code)
	}
}

func TestAuthenticatedDashboardReadsRealPG18(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("DBSPHERE_TEST_PG18_URL is not set")
	}
	pg, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	hash, err := security.HashPassword("dashboard-test-password")
	if err != nil {
		t.Fatal(err)
	}
	handler := New(pg, AuthConfig{MasterKey: []byte("01234567890123456789012345678901"), PasswordHash: hash})
	login := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"password":"dashboard-test-password"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(login, request)
	if login.Code != http.StatusOK {
		t.Fatalf("dashboard login failed: %d", login.Code)
	}
	for _, path := range []string{"/api/assets", "/api/events", "/api/alerts", "/api/alert-rules",
		"/api/silences", "/api/notifications", "/api/storage", "/api/inspection-templates",
		"/api/inspection-schedules", "/api/inspection-runs"} {
		record := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.AddCookie(login.Result().Cookies()[0])
		handler.ServeHTTP(record, request)
		if record.Code != http.StatusOK || !strings.Contains(record.Body.String(), `":`) {
			t.Fatalf("dashboard read %s failed: %d %s", path, record.Code, record.Body.String())
		}
	}
	var credentials struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &credentials); err != nil {
		t.Fatal(err)
	}
	input := `{"name":"Go HTTP test","host":"127.0.0.1","port":65534,"database":"postgres",
		"username":"dbsphere_monitor","password":"test-secret","sslMode":"disable","runtime":"linux"}`
	createRequest := httptest.NewRequest(http.MethodPost, "/api/assets", strings.NewReader(input))
	createRequest.AddCookie(login.Result().Cookies()[0])
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("X-CSRF-Token", credentials.CSRF)
	created := httptest.NewRecorder()
	handler.ServeHTTP(created, createRequest)
	if created.Code != http.StatusCreated {
		t.Fatalf("asset create failed: %d %s", created.Code, created.Body.String())
	}
	var result struct {
		Asset struct {
			ID string `json:"id"`
		} `json:"asset"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &result); err != nil || result.Asset.ID == "" {
		t.Fatal("created asset ID missing", err)
	}
	defer pg.DeleteAsset(context.Background(), result.Asset.ID)
	if strings.Contains(created.Body.String(), "test-secret") || strings.Contains(created.Body.String(), "password_enc") {
		t.Fatal("asset response leaked credential")
	}
	getRequest := httptest.NewRequest(http.MethodGet, "/api/assets/"+result.Asset.ID, nil)
	getRequest.AddCookie(login.Result().Cookies()[0])
	got := httptest.NewRecorder()
	handler.ServeHTTP(got, getRequest)
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "Go HTTP test") {
		t.Fatal("asset read failed")
	}
	checkRequest := httptest.NewRequest(http.MethodPost, "/api/assets/"+result.Asset.ID+"/check", nil)
	checkRequest.AddCookie(login.Result().Cookies()[0])
	checkRequest.Header.Set("X-CSRF-Token", credentials.CSRF)
	checked := httptest.NewRecorder()
	handler.ServeHTTP(checked, checkRequest)
	if checked.Code != http.StatusOK || !strings.Contains(checked.Body.String(), `"status":"error"`) {
		t.Fatalf("Go collection failure not recorded: %d %s", checked.Code, checked.Body.String())
	}
	var checkResult struct {
		Result struct {
			SampleID string `json:"sampleId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(checked.Body.Bytes(), &checkResult); err != nil {
		t.Fatal(err)
	}
	eventsRequest := httptest.NewRequest(http.MethodGet, "/api/events", nil)
	eventsRequest.AddCookie(login.Result().Cookies()[0])
	events := httptest.NewRecorder()
	handler.ServeHTTP(events, eventsRequest)
	var eventList struct {
		Events []struct{ ID, AssetID string } `json:"events"`
	}
	if err := json.Unmarshal(events.Body.Bytes(), &eventList); err != nil {
		t.Fatal(err)
	}
	eventID := ""
	for _, event := range eventList.Events {
		if event.AssetID == result.Asset.ID {
			eventID = event.ID
			break
		}
	}
	if eventID == "" {
		t.Fatal("collection failure event missing")
	}
	eventRequest := httptest.NewRequest(http.MethodGet, "/api/events/"+eventID, nil)
	eventRequest.AddCookie(login.Result().Cookies()[0])
	eventDetail := httptest.NewRecorder()
	handler.ServeHTTP(eventDetail, eventRequest)
	if eventDetail.Code != http.StatusOK || !strings.Contains(eventDetail.Body.String(), "observed_in") {
		t.Fatalf("one-event evidence graph unavailable: %d %s", eventDetail.Code, eventDetail.Body.String())
	}
	reportPath := "/api/events/" + eventID + "/report"
	unauthenticatedReport := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedReport, httptest.NewRequest(http.MethodGet, reportPath, nil))
	if unauthenticatedReport.Code != http.StatusUnauthorized {
		t.Fatalf("event report was exposed without login: %d", unauthenticatedReport.Code)
	}
	reportRequest := httptest.NewRequest(http.MethodGet, reportPath+"?format=md", nil)
	reportRequest.AddCookie(login.Result().Cookies()[0])
	report := httptest.NewRecorder()
	handler.ServeHTTP(report, reportRequest)
	if report.Code != http.StatusOK || !strings.Contains(report.Body.String(), eventID) ||
		!strings.Contains(report.Body.String(), checkResult.Result.SampleID) ||
		strings.Contains(report.Body.String(), "test-secret") ||
		!strings.Contains(report.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("event report did not retain safe evidence: %d %s", report.Code, report.Body.String())
	}
	decisionPath := "/api/events/" + eventID + "/rca-decision"
	decisionRequest := httptest.NewRequest(http.MethodPost, decisionPath,
		strings.NewReader(`{"code":"unknown","decision":"confirmed","reason":"no supporting evidence"}`))
	decisionRequest.AddCookie(login.Result().Cookies()[0])
	decisionRequest.Header.Set("Content-Type", "application/json")
	withoutCSRF := httptest.NewRecorder()
	handler.ServeHTTP(withoutCSRF, decisionRequest)
	if withoutCSRF.Code != http.StatusForbidden {
		t.Fatalf("RCA review accepted without CSRF: %d", withoutCSRF.Code)
	}
	decisionRequest.Header.Set("X-CSRF-Token", credentials.CSRF)
	unsupported := httptest.NewRecorder()
	handler.ServeHTTP(unsupported, decisionRequest)
	if unsupported.Code != http.StatusBadRequest {
		t.Fatalf("unsupported RCA candidate accepted: %d %s", unsupported.Code, unsupported.Body.String())
	}
	sampleRequest := httptest.NewRequest(http.MethodGet, "/api/assets/"+result.Asset.ID+"/samples/"+checkResult.Result.SampleID, nil)
	if !rawSamplePath.MatchString(sampleRequest.URL.Path) {
		t.Fatalf("sample route does not match %q", sampleRequest.URL.Path)
	}
	sampleRequest.AddCookie(login.Result().Cookies()[0])
	sample := httptest.NewRecorder()
	handler.ServeHTTP(sample, sampleRequest)
	if sample.Code != http.StatusOK || !strings.Contains(sample.Body.String(), checkResult.Result.SampleID) {
		t.Fatalf("raw sample link failed: %d sample=%q body=%s", sample.Code, checkResult.Result.SampleID, sample.Body.String())
	}
	seriesQuery := "?from=" + neturl.QueryEscape(time.Now().Add(-24*time.Hour).UTC().Format(time.RFC3339Nano)) +
		"&to=" + neturl.QueryEscape(time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano))
	for _, section := range []string{"samples", "sql", "slow-logs", "sql-plans", "capacity", "series" + seriesQuery} {
		request := httptest.NewRequest(http.MethodGet, "/api/assets/"+result.Asset.ID+"/"+section, nil)
		request.AddCookie(login.Result().Cookies()[0])
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("asset section %s failed: %d %s", section, response.Code, response.Body.String())
		}
	}
	missingLog := "/api/assets/" + result.Asset.ID + "/slow-logs/00000000-0000-0000-0000-000000000001"
	if !rawSQLPath.MatchString(missingLog) {
		t.Fatal("raw SQL evidence route does not match a valid UUID")
	}
	missingLogRequest := httptest.NewRequest(http.MethodGet, missingLog, nil)
	missingLogRequest.AddCookie(login.Result().Cookies()[0])
	missingLogResponse := httptest.NewRecorder()
	handler.ServeHTTP(missingLogResponse, missingLogRequest)
	if missingLogResponse.Code != http.StatusNotFound {
		t.Fatalf("unknown SQL evidence should return 404, got %d", missingLogResponse.Code)
	}
	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/assets/"+result.Asset.ID, nil)
	deleteRequest.AddCookie(login.Result().Cookies()[0])
	deleteRequest.Header.Set("X-CSRF-Token", credentials.CSRF)
	deleted := httptest.NewRecorder()
	handler.ServeHTTP(deleted, deleteRequest)
	if deleted.Code != http.StatusOK {
		t.Fatalf("asset delete failed: %d", deleted.Code)
	}
}
