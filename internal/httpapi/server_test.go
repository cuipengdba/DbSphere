package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/cuipengdba/dbsphere/internal/store"
)

type healthStub struct{ err error }

func (stub healthStub) Health(context.Context) error { return stub.err }

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
