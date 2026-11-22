package httpapi

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/ai"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/cuipengdba/dbsphere/internal/store"
	"golang.org/x/oauth2"
)

func TestPG18OrganizationIsolationOIDCFallbackAndSignedAudit(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if dsn == "" {
		t.Skip("PostgreSQL 18 is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	pg, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	const rootPassword = "organization-root-fixture"
	const tenantPassword = "organization-tenant-fixture"
	rootHash, err := security.HashPassword(rootPassword)
	if err != nil {
		t.Fatal(err)
	}

	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var provider *httptest.Server
	var acceptedNonce, challenge string
	var codeAuthOK atomic.Bool
	sendToken := func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.Form.Get("code") != "fixture-code" || r.Form.Get("code_verifier") == "" ||
			oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != challenge {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		codeAuthOK.Store(true)
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"fixture","typ":"JWT"}`))
		claims, _ := json.Marshal(map[string]any{"iss": provider.URL, "sub": "external-subject-a", "aud": "dbsphere-fixture",
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(), "nonce": acceptedNonce})
		content := header + "." + base64.RawURLEncoding.EncodeToString(claims)
		hash := sha256.Sum256([]byte(content))
		signature, err := rsa.SignPKCS1v15(rand.Reader, private, crypto.SHA256, hash[:])
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "unused-fixture-token", "token_type": "Bearer",
			"id_token": content + "." + base64.RawURLEncoding.EncodeToString(signature)})
	}
	provider = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": provider.URL, "authorization_endpoint": provider.URL + "/authorize",
				"token_endpoint": provider.URL + "/token", "jwks_uri": provider.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "kid": "fixture", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(private.PublicKey.N.Bytes()), "e": "AQAB"}}})
		case "/token":
			sendToken(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()
	t.Setenv("DBSPHERE_OIDC_ISSUER", provider.URL)
	t.Setenv("DBSPHERE_OIDC_CLIENT_ID", "dbsphere-fixture")
	t.Setenv("DBSPHERE_OIDC_CLIENT_SECRET", "test-OIDC-secret")
	t.Setenv("DBSPHERE_OIDC_REDIRECT_URL", "http://127.0.0.1:18085/api/oidc/callback")
	oidcConfig, err := LoadOIDCProvider()
	if err != nil {
		t.Fatal(err)
	}
	var evidenceA, eventB string
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct{ Messages []struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		combined := ""
		for _, message := range request.Messages {
			combined += message.Content
		}
		if !strings.Contains(combined, evidenceA) || strings.Contains(combined, eventB) {
			t.Error("The model received missing or cross-organization evidence")
		}
		calls.Add(1)
		answer, _ := json.Marshal(map[string]any{"summary": "Verified evidence", "evidenceIds": []string{evidenceA},
			"uncertainty": "Needs Linux corroboration", "suggestedChecks": []string{"Review the original sample"}})
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"finish_reason": "stop",
			"message": map[string]string{"content": string(answer)}}}})
	}))
	defer model.Close()
	aiClient, err := ai.New(ai.Config{URL: model.URL + "/v1/chat/completions", Model: "tenant-fixture"})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(pg, AuthConfig{PasswordHash: rootHash, MasterKey: key, OIDC: oidcConfig, AI: aiClient})

	type session struct {
		Cookie *http.Cookie
		CSRF   string
	}
	call := func(session session, method, path string, data any, status int) *httptest.ResponseRecorder {
		t.Helper()
		var body string
		if data != nil {
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			body = string(raw)
		}
		request := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		if session.Cookie != nil {
			request.AddCookie(session.Cookie)
			request.Header.Set("X-CSRF-Token", session.CSRF)
		}
		result := httptest.NewRecorder()
		handler.ServeHTTP(result, request)
		if result.Code != status {
			t.Fatalf("%s %s: expected %d, got %d: %s", method, path, status, result.Code, result.Body.String())
		}
		return result
	}
	decode := func(result *httptest.ResponseRecorder, dest any) {
		t.Helper()
		if err := json.Unmarshal(result.Body.Bytes(), dest); err != nil {
			t.Fatalf("%v: %s", err, result.Body.String())
		}
	}
	login := func(username, password string) session {
		result := call(session{}, "POST", "/api/login", map[string]string{"username": username, "password": password}, 200)
		var auth session
		decode(result, &auth)
		auth.Cookie = result.Result().Cookies()[0]
		return auth
	}
	root := login("", rootPassword)
	suffix := time.Now().UTC().Format("150405.000000000")
	organization := func(name string) string {
		var row struct{ Organization struct{ ID string } }
		decode(call(root, "POST", "/api/admin/organizations", map[string]string{"name": name + " " + suffix}, 201), &row)
		return row.Organization.ID
	}
	organizationA, organizationB := organization("Tenant A"), organization("Tenant B")
	createAccount := func(username, organization, subject string) string {
		var row struct{ Account struct{ ID string } }
		decode(call(root, "POST", "/api/admin/accounts", map[string]any{"username": username, "role": "admin",
			"password": tenantPassword, "organizationId": organization, "oidcSubject": subject}, 201), &row)
		return row.Account.ID
	}
	usernameA, usernameB := "org-a-"+suffix, "org-b-"+suffix
	_ = createAccount(usernameA, organizationA, "external-subject-a")
	accountB := createAccount(usernameB, organizationB, "")
	tenantA, tenantB := login(usernameA, tenantPassword), login(usernameB, tenantPassword)
	type eventFixture struct{ Asset, Event, Sample string }
	var createdAssets []string
	defer func() {
		for _, id := range createdAssets {
			_, _ = pg.DeleteAsset(context.Background(), id)
		}
	}()
	createAsset := func(auth session, label string) eventFixture {
		t.Helper()
		var row struct{ Asset struct{ ID string } }
		decode(call(auth, "POST", "/api/assets", map[string]any{"name": label + " " + suffix, "host": "127.0.0.1", "port": 1,
			"database": "postgres", "username": "monitor", "password": "database-fixture-password",
			"sslMode": "disable", "runtime": "linux"}, 201), &row)
		fixture := eventFixture{Asset: row.Asset.ID}
		var check struct{ Result struct{ SampleID string } }
		decode(call(auth, "POST", "/api/assets/"+fixture.Asset+"/check", nil, 200), &check)
		fixture.Sample = check.Result.SampleID
		var events struct {
			Events []struct{ ID, AssetID string }
		}
		decode(call(auth, "GET", "/api/events", nil, 200), &events)
		for _, event := range events.Events {
			if event.AssetID == fixture.Asset {
				fixture.Event = event.ID
				break
			}
		}
		if fixture.Event == "" || fixture.Sample == "" {
			t.Fatal("fixture has no incident and source sample")
		}
		createdAssets = append(createdAssets, fixture.Asset)
		return fixture
	}
	a, b := createAsset(tenantA, "Tenant A private asset"), createAsset(tenantB, "Tenant B private asset")
	eventB = b.Event
	_, ids, err := pg.EventAIEvidence(ctx, a.Event)
	if err != nil || len(ids) == 0 {
		t.Fatal("Missing AI evidence", err)
	}
	evidenceA = ids[0]

	for _, scope := range []struct {
		Auth       session
		Own, Other eventFixture
	}{{tenantA, a, b}, {tenantB, b, a}} {
		for _, path := range []string{"/api/assets", "/api/assets/page?limit=10", "/api/events/page?limit=10", "/api/alerts", "/api/access/assets/export"} {
			body := call(scope.Auth, "GET", path, nil, 200).Body.String()
			if !strings.Contains(body, scope.Own.Asset) || strings.Contains(body, scope.Other.Asset) {
				t.Fatalf("Incorrect asset scope through %s: %s", path, body)
			}
		}
		now := time.Now().UTC()
		rangeQuery := "&bucketMinutes=1&from=" + url.QueryEscape(now.Add(-time.Hour).Format(time.RFC3339)) + "&to=" + url.QueryEscape(now.Format(time.RFC3339))
		call(scope.Auth, "GET", "/api/monitoring/series?asset="+scope.Own.Asset+rangeQuery, nil, 200)
		call(scope.Auth, "GET", "/api/monitoring/series?asset="+scope.Other.Asset, nil, 404)
		call(scope.Auth, "GET", "/api/assets/"+scope.Own.Asset+"/samples/"+scope.Own.Sample, nil, 200)
		call(scope.Auth, "GET", "/api/assets/"+scope.Own.Asset+"/samples/"+scope.Other.Sample, nil, 404)
		call(scope.Auth, "GET", "/api/assets/"+scope.Other.Asset+"/samples/"+scope.Other.Sample, nil, 404)
		call(scope.Auth, "GET", "/api/events/"+scope.Other.Event+"/report", nil, 404)
		call(scope.Auth, "GET", "/api/events/"+scope.Other.Event+"/audit", nil, 404)
		call(scope.Auth, "POST", "/api/events/"+scope.Other.Event+"/ai-summary", nil, 404)
	}
	call(tenantA, "PATCH", "/api/admin/accounts/"+accountB, map[string]bool{"enabled": false}, 404)
	call(tenantA, "POST", "/api/admin/accounts/"+accountB+"/rotate", map[string]string{"password": "unauthorized-fixture"}, 404)
	if calls.Load() != 0 {
		t.Fatal("Cross-org AI calls reached the model")
	}
	call(tenantA, "POST", "/api/events/"+a.Event+"/ai-summary", nil, 200)
	if calls.Load() != 1 {
		t.Fatal("Authorized model request was not processed")
	}

	var share struct{ Share struct{ ID string } }
	path := "/api/events/" + a.Event + "/shares"
	decode(call(tenantA, "POST", path, map[string]int{"hours": 1}, 201), &share)
	sharePath := "/api/event-shares/" + share.Share.ID
	call(tenantB, "GET", sharePath, nil, 404)
	call(tenantA, "GET", sharePath, nil, 200)
	call(tenantA, "DELETE", path+"/"+share.Share.ID, nil, 200)
	call(tenantA, "GET", sharePath, nil, 404)

	var export security.AuditExport
	auditPath := "/api/admin/organization-audit"
	decode(call(tenantA, "GET", auditPath, nil, 200), &export)
	if export.OrganizationID != organizationA || export.HeadSequence < 2 {
		t.Fatalf("Missing isolated audit: %+v", export)
	}
	if err := security.VerifyAuditExport(export, export.KeyFingerprint); err != nil {
		t.Fatal(err)
	}
	call(tenantA, "GET", auditPath+"?organizationId="+organizationB, nil, 404)
	bytes, _ := json.Marshal(export)
	if strings.Contains(string(bytes), b.Asset) || strings.Contains(string(bytes), b.Event) || strings.Contains(string(bytes), "database-fixture-password") {
		t.Fatal("Another tenant or database password leaked to the signed audit")
	}
	export.Entries[0].Record = json.RawMessage(`{"tampered":true}`)
	if security.VerifyAuditExport(export, export.KeyFingerprint) == nil {
		t.Fatal("Changed audit body verified")
	}

	oauthStart := call(session{}, "GET", "/api/oidc/start", nil, 303)
	target, err := url.Parse(oauthStart.Header().Get("Location"))
	if err != nil || target.Host == "" {
		t.Fatal("OIDC authorization redirect missing", err)
	}
	acceptedNonce, challenge = target.Query().Get("nonce"), target.Query().Get("code_challenge")
	if acceptedNonce == "" || challenge == "" || target.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("OIDC protections missing")
	}
	flow := session{Cookie: oauthStart.Result().Cookies()[0]}
	callback := oidcCallbackPath + "?code=fixture-code&state=" + url.QueryEscape(target.Query().Get("state"))
	success := call(flow, "GET", callback, nil, 303)
	if success.Header().Get("Location") != "/" || !codeAuthOK.Load() {
		t.Fatal("OIDC login did not complete")
	}
	var oidcSession session
	for _, cookie := range success.Result().Cookies() {
		if cookie.Name == "dbsphere_session" {
			oidcSession.Cookie = cookie
		}
	}
	if oidcSession.Cookie == nil {
		t.Fatal("OIDC callback did not grant a session")
	}
	replayed := call(flow, "GET", callback, nil, 303)
	if replayed.Header().Get("Location") != "/?sso=rejected" {
		t.Fatal("Consumed OIDC state was replayed")
	}
	info := call(oidcSession, "GET", "/api/session", nil, 200).Body.String()
	if !strings.Contains(info, organizationA) || !strings.Contains(info, usernameA) {
		t.Fatal("OIDC session lost scope")
	}

	provider.Close()
	if result := call(session{}, "GET", "/api/oidc/start", nil, 303); result.Header().Get("Location") != "/?sso=unavailable" {
		t.Fatal("OIDC outage was not exposed")
	}
	fallback := login(usernameA, tenantPassword)
	call(fallback, "GET", "/api/events/"+a.Event, nil, 200)
	call(fallback, "GET", "/api/events/"+b.Event, nil, 404)
	t.Logf("organizationA=%s organizationB=%s eventA=%s eventB=%s signed=%d sso=true fallback=true", organizationA, organizationB, a.Event, b.Event, export.HeadSequence)
}
