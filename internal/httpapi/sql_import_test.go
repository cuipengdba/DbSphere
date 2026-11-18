package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cuipengdba/dbsphere/internal/security"
	storepkg "github.com/cuipengdba/dbsphere/internal/store"
)

type importStub struct {
	healthStub
	logs, plans int
}

func (s *importStub) ImportSlowLogs(_ context.Context, _, format, content string) (any, error) {
	s.logs++
	if format != "jsonlog" || content != "test" {
		return nil, storepkg.ErrInvalidSlowLog
	}
	return map[string]int{"scanned": 1, "ignored": 0, "imported": 1, "duplicates": 0}, nil
}
func (s *importStub) ImportSQLPlan(_ context.Context, _, queryID, _ string, plan json.RawMessage) (any, error) {
	s.plans++
	if queryID != "42" || len(plan) == 0 {
		return nil, storepkg.ErrInvalidSQLPlan
	}
	return json.RawMessage(`{"queryId":"42"}`), nil
}

func TestGoSQLImportsRequireSessionCSRFAndBoundedJSON(t *testing.T) {
	hash, err := security.HashPassword("import-test-password")
	if err != nil {
		t.Fatal(err)
	}
	stub := &importStub{}
	handler := New(stub, AuthConfig{MasterKey: []byte("01234567890123456789012345678901"), PasswordHash: hash})
	const base = "/api/assets/11111111-1111-1111-1111-111111111111"
	call := func(path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
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
	if got := call(base+"/slow-logs/import", `{"format":"jsonlog","content":"test"}`, nil, ""); got.Code != http.StatusUnauthorized {
		t.Fatal("anonymous SQL import accepted")
	}
	login := call("/api/login", `{"password":"import-test-password"}`, nil, "")
	if login.Code != http.StatusOK {
		t.Fatalf("login failed: %d", login.Code)
	}
	var session struct {
		CSRF string `json:"csrf"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	if got := call(base+"/slow-logs/import", `{"format":"jsonlog","content":"test"}`, login.Result().Cookies()[0], ""); got.Code != http.StatusForbidden {
		t.Fatal("SQL import without CSRF accepted")
	}
	if got := call(base+"/slow-logs/import", `{"format":"jsonlog","content":"test"}`, login.Result().Cookies()[0], session.CSRF); got.Code != http.StatusOK {
		t.Fatalf("slow log route failed: %d %s", got.Code, got.Body.String())
	}
	if got := call(base+"/sql-plans", `{"queryId":"42","observedAt":"2026-11-18T00:00:00Z","plan":{"Plan":{"Node Type":"Result"}}}`, login.Result().Cookies()[0], session.CSRF); got.Code != http.StatusCreated {
		t.Fatalf("plan route failed: %d %s", got.Code, got.Body.String())
	}
	if got := call(base+"/slow-logs/import", `{"format":"jsonlog","content":"`+strings.Repeat("x", 130<<10)+`"}`, login.Result().Cookies()[0], session.CSRF); got.Code != http.StatusBadRequest {
		t.Fatalf("oversized import accepted: %d", got.Code)
	}
	if stub.logs != 1 || stub.plans != 1 {
		t.Fatal("invalid or unauthorized request reached storage")
	}
}
