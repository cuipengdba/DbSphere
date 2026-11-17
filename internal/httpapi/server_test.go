package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
}
