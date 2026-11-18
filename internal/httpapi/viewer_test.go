package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cuipengdba/dbsphere/internal/security"
)

type viewerStub struct{ silenceStub }

func (*viewerStub) ReadCollection(_ context.Context, path string) (any, bool, error) {
	if path == "/api/assets" {
		return map[string]any{"assets": []any{}}, true, nil
	}
	return nil, false, nil
}

func TestViewerCanReadButCannotMutate(t *testing.T) {
	adminHash, err := security.HashPassword("admin-role-test-password")
	if err != nil {
		t.Fatal(err)
	}
	viewerHash, err := security.HashPassword("viewer-role-test-password")
	if err != nil {
		t.Fatal(err)
	}
	stub := &viewerStub{}
	handler := New(stub, AuthConfig{MasterKey: []byte("01234567890123456789012345678901"),
		PasswordHash: adminHash, ViewerPasswordHash: viewerHash})
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
	login := call(http.MethodPost, "/api/login", `{"password":"viewer-role-test-password"}`, nil, "")
	if login.Code != http.StatusOK {
		t.Fatalf("viewer login: %d", login.Code)
	}
	var session struct{ Role, CSRF string }
	if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil || session.Role != "viewer" {
		t.Fatal("viewer role missing from login", err)
	}
	cookie := login.Result().Cookies()[0]
	if got := call(http.MethodGet, "/api/session", "", cookie, ""); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"role":"viewer"`) {
		t.Fatal("viewer session lost role")
	}
	if got := call(http.MethodGet, "/api/assets", "", cookie, ""); got.Code != http.StatusOK {
		t.Fatalf("viewer read denied: %d", got.Code)
	}
	rotatedHash, err := security.HashPassword("rotated-viewer-role-password")
	if err != nil {
		t.Fatal(err)
	}
	rotated := New(stub, AuthConfig{MasterKey: []byte("01234567890123456789012345678901"),
		PasswordHash: adminHash, ViewerPasswordHash: rotatedHash})
	oldViewerRequest := httptest.NewRequest(http.MethodGet, "/api/assets", nil)
	oldViewerRequest.AddCookie(cookie)
	oldViewerResponse := httptest.NewRecorder()
	rotated.ServeHTTP(oldViewerResponse, oldViewerRequest)
	if oldViewerResponse.Code != http.StatusUnauthorized {
		t.Fatal("old viewer session survived password rotation")
	}
	if got := call(http.MethodPost, "/api/silences", `{"reason":"valid"}`, cookie, session.CSRF); got.Code != http.StatusForbidden || stub.created != 0 {
		t.Fatal("viewer changed monitoring data")
	}
	if got := call(http.MethodPost, "/api/logout", "", cookie, session.CSRF); got.Code != http.StatusOK {
		t.Fatal("viewer logout failed")
	}
	admin := call(http.MethodPost, "/api/login", `{"password":"admin-role-test-password"}`, nil, "")
	var adminSession struct{ Role, CSRF string }
	if err := json.Unmarshal(admin.Body.Bytes(), &adminSession); err != nil || adminSession.Role != "admin" {
		t.Fatal("admin role missing", err)
	}
	if got := call(http.MethodPost, "/api/silences", `{"reason":"valid"}`, admin.Result().Cookies()[0], adminSession.CSRF); got.Code != http.StatusCreated || stub.created != 1 {
		t.Fatal("administrator write was denied")
	}
}
