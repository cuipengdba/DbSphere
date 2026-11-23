package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/cuipengdba/dbsphere/internal/security"
)

type setupAdministratorStub struct{ hash string }

func (s *setupAdministratorStub) CurrentAdminPasswordHash(context.Context) (string, error) {
	return s.hash, nil
}

func (s *setupAdministratorStub) CompleteAdministratorSetup(_ context.Context, password string) (string, error) {
	if s.hash != "" {
		return "", nil
	}
	hash, err := security.HashPassword(password)
	s.hash = hash
	return hash, err
}

func TestInitialAdministratorOnlyAcquiresSessionAfterPasswordChange(t *testing.T) {
	store := &setupAdministratorStub{}
	h := New(healthStub{}, AuthConfig{Administrator: store, MasterKey: []byte("01234567890123456789012345678901")})
	call := func(method, path, body, remote, authority string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr, r.Host = remote, authority
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	const local = "127.0.0.1:19000"
	const authority = "127.0.0.1:8080"
	const remote = "192.0.2.4:19000"
	initial := map[string]string{"username": security.InitialAdministrator, "password": security.InitialAdministratorPassword}
	payload, _ := json.Marshal(initial)
	if w := call("GET", "/api/assets", "", local, authority); w.Code != http.StatusUnauthorized {
		t.Fatalf("assets were accessible before setup: %d", w.Code)
	}
	for _, source := range []struct{ remote, host string }{{remote, authority}, {local, "ops.example.net"}} {
		if w := call("POST", "/api/login", string(payload), source.remote, source.host); w.Code != http.StatusForbidden {
			t.Fatalf("non-local setup was accepted: %d", w.Code)
		}
	}
	probe := call("POST", "/api/login", string(payload), local, authority)
	if probe.Code != http.StatusOK || !strings.Contains(probe.Body.String(), `"setupRequired":true`) || len(probe.Result().Cookies()) != 0 {
		t.Fatalf("initial password did not produce a setup-only response: %d %s", probe.Code, probe.Body.String())
	}
	initial["newPassword"] = "administrator-fixture-password"
	configured, _ := json.Marshal(initial)
	saved := call("POST", "/api/login", string(configured), local, authority)
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"authenticated":true`) || len(saved.Result().Cookies()) != 1 {
		t.Fatalf("saved password did not grant a real session: %d %s", saved.Code, saved.Body.String())
	}
	if w := call("POST", "/api/login", string(payload), local, authority); w.Code != http.StatusUnauthorized {
		t.Fatalf("initial password remained valid: %d", w.Code)
	}
	if w := call("POST", "/api/login", string(configured), local, authority); w.Code != http.StatusConflict {
		t.Fatalf("a second setup request was accepted: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/api/session", nil)
	r.AddCookie(saved.Result().Cookies()[0])
	w := httptest.NewRecorder()
	New(healthStub{}, AuthConfig{Administrator: store, MasterKey: []byte("01234567890123456789012345678901")}).ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"role":"admin"`) {
		t.Fatalf("the stored password was not used after handler restart: %d %s", w.Code, w.Body.String())
	}
}

func TestLocalSetupIgnoresUntrustedForwarding(t *testing.T) {
	bridge := netip.MustParseAddr("172.19.0.1")
	r := httptest.NewRequest("POST", "http://127.0.0.1:8081/api/login", nil)
	r.RemoteAddr = "172.19.0.1:10000"
	if !localSetupAllowed(r, bridge) {
		t.Fatal("Docker loopback publisher was rejected")
	}
	r.RemoteAddr = "172.19.0.4:10000"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	if localSetupAllowed(r, netip.Addr{}) {
		t.Fatal("a non-local TCP peer was trusted based on its forwarding headers")
	}
	r.RemoteAddr = "127.0.0.1:10000"
	r.Header.Set("X-Forwarded-For", "198.51.100.8")
	if localSetupAllowed(r, netip.Addr{}) {
		t.Fatal("a non-local request forwarded by a local proxy was accepted")
	}
}
