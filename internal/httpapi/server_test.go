package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
