package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/security"
)

type adminAuditStub struct {
	healthStub
	method, path   string
	status, starts int
	fail           bool
}

func (s *adminAuditStub) StartAdminAudit(_ context.Context, method, path string) (string, error) {
	s.starts++
	if s.fail {
		return "", errors.New("audit database unavailable")
	}
	s.method, s.path = method, path
	return "audit-id", nil
}
func (s *adminAuditStub) FinishAdminAudit(_ context.Context, _ string, status int) error {
	s.status = status
	return nil
}
func (*adminAuditStub) RecentAdminAudit(context.Context) (any, error) {
	return map[string]any{"entries": []any{}}, nil
}
func (*adminAuditStub) RecentEventAdminAudit(context.Context, string) (any, error) {
	return map[string]any{"entries": []any{}}, nil
}

func TestAdministratorMutationAuditAndViewerBoundary(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	viewerHash, err := security.HashPassword("read-only-password")
	if err != nil {
		t.Fatal(err)
	}
	stub := &adminAuditStub{}
	handler := New(stub, AuthConfig{MasterKey: key, ViewerPasswordHash: viewerHash})
	adminToken, err := security.CreateSession(security.AdminSessionKey(key, ""), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	viewerKey := security.ViewerSessionKey(key, viewerHash)
	viewerToken, err := security.CreateViewerSession(viewerKey, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader("{}"))
		r.AddCookie(&http.Cookie{Name: "dbsphere_session", Value: token})
		if method != http.MethodGet {
			r.Header.Set("X-CSRF-Token", security.CSRFToken(key, token))
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if got := call(http.MethodGet, "/api/admin/audit", viewerToken); got.Code != http.StatusForbidden {
		t.Fatalf("viewer read audit: %d", got.Code)
	}
	if got := call(http.MethodGet, "/api/events/11111111-1111-1111-1111-111111111111/audit", viewerToken); got.Code != http.StatusForbidden {
		t.Fatalf("viewer read event audit: %d", got.Code)
	}
	if got := call(http.MethodPost, "/api/assets", viewerToken); got.Code != http.StatusForbidden || stub.starts != 0 {
		t.Fatalf("viewer mutation was audited/run: %d", got.Code)
	}
	if got := call(http.MethodGet, "/api/admin/audit", adminToken); got.Code != http.StatusOK {
		t.Fatalf("admin read audit: %d", got.Code)
	}
	if got := call(http.MethodGet, "/api/events/11111111-1111-1111-1111-111111111111/audit", adminToken); got.Code != http.StatusOK {
		t.Fatalf("admin read event audit: %d", got.Code)
	}
	if got := call(http.MethodPut, "/api/inspection-schedules", adminToken); got.Code != http.StatusNotImplemented || stub.starts != 1 || stub.status != http.StatusNotImplemented || stub.method != "PUT" || stub.path != "/api/inspection-schedules" {
		t.Fatalf("PUT not audited: response=%d starts=%d status=%d method=%s path=%s", got.Code, stub.starts, stub.status, stub.method, stub.path)
	}
	stub.fail = true
	if got := call(http.MethodPost, "/api/assets", adminToken); got.Code != http.StatusServiceUnavailable || !strings.Contains(got.Body.String(), "operation was not run") {
		t.Fatalf("audit failure did not stop mutation: %d %s", got.Code, got.Body.String())
	}
}
