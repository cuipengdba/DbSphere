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

const scopeAssetA = "11111111-1111-1111-1111-111111111111"
const scopeAssetB = "22222222-2222-2222-2222-222222222222"
const scopeEventA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const scopeEventB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

type scopedViewerStub struct {
	collections    map[string]any
	pageScope      []string
	overviewScope  []string
	assetPageScope []string
}

func (*scopedViewerStub) Health(context.Context) error { return nil }

func (s *scopedViewerStub) ReadCollection(_ context.Context, path string) (any, bool, error) {
	value, found := s.collections[path]
	return value, found, nil
}

func (s *scopedViewerStub) ReadScopedCollection(ctx context.Context, path string, _ []string) (any, bool, error) {
	return s.ReadCollection(ctx, path)
}
func (s *scopedViewerStub) EventPage(_ context.Context, query storepkg.EventPageQuery, assetIDs []string) (any, error) {
	s.pageScope = append([]string(nil), assetIDs...)
	return map[string]any{"events": []any{map[string]any{"id": scopeEventA,
		"assetId": scopeAssetA}}, "hasMore": false, "nextCursor": ""}, nil
}
func (s *scopedViewerStub) EventOverview(_ context.Context, assetIDs []string) (any, error) {
	s.overviewScope = append([]string(nil), assetIDs...)
	return map[string]any{"openCount": 1, "latestOpen": []any{map[string]any{
		"id": scopeEventA, "assetId": scopeAssetA}}}, nil
}
func (s *scopedViewerStub) AssetPage(_ context.Context, query storepkg.AssetPageQuery, assetIDs []string) (any, error) {
	if query.Runtime == "bad" {
		return nil, storepkg.ErrInvalidAssetPage
	}
	s.assetPageScope = append([]string(nil), assetIDs...)
	return map[string]any{"assets": []any{map[string]any{"id": scopeAssetA}},
		"hasMore": false, "nextCursor": ""}, nil
}

func (*scopedViewerStub) AssetIDForResource(_ context.Context, kind, id string) (string, error) {
	if kind == "event" {
		switch id {
		case scopeEventA:
			return scopeAssetA, nil
		case scopeEventB:
			return scopeAssetB, nil
		}
	}
	if kind == "inspection" && id == scopeEventB {
		return scopeAssetB, nil
	}
	return "", nil
}

func TestScopedViewerCannotReadOtherAssetsOrAggregates(t *testing.T) {
	adminHash, err := security.HashPassword("scoped-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	viewerHash, err := security.HashPassword("scoped-viewer-password")
	if err != nil {
		t.Fatal(err)
	}
	stub := &scopedViewerStub{collections: map[string]any{
		"/api/assets": map[string]any{"assets": []any{
			map[string]any{"id": scopeAssetA, "name": "visible"},
			map[string]any{"id": scopeAssetB, "name": "hidden"}}},
		"/api/events": map[string]any{"events": []any{
			map[string]any{"assetId": scopeAssetA, "id": scopeEventA},
			map[string]any{"assetId": scopeAssetB, "id": scopeEventB}}},
		"/api/notifications": map[string]any{"notifications": []any{
			map[string]any{"assetId": scopeAssetB, "lastError": "private"}}},
		"/api/alert-rules": map[string]any{"rules": []any{}},
	}}
	handler := New(stub, AuthConfig{MasterKey: []byte("01234567890123456789012345678901"),
		PasswordHash: adminHash, ViewerPasswordHash: viewerHash, ViewerAssetIDs: []string{scopeAssetA}})
	login := httptest.NewRecorder()
	loginRequest := httptest.NewRequest(http.MethodPost, "/api/login",
		strings.NewReader(`{"password":"scoped-viewer-password"}`))
	loginRequest.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("viewer login: %d %s", login.Code, login.Body.String())
	}
	cookie := login.Result().Cookies()[0]
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	for _, path := range []string{"/api/assets", "/api/events"} {
		got := get(path)
		if got.Code != http.StatusOK || strings.Contains(got.Body.String(), scopeAssetB) ||
			!strings.Contains(got.Body.String(), scopeAssetA) {
			t.Fatalf("scope leaked through %s: %d %s", path, got.Code, got.Body.String())
		}
	}
	if got := get("/api/events/page?limit=20"); got.Code != http.StatusOK ||
		strings.Contains(got.Body.String(), scopeAssetB) || !strings.Contains(got.Body.String(), scopeAssetA) ||
		len(stub.pageScope) != 1 || stub.pageScope[0] != scopeAssetA {
		t.Fatalf("event page scope missing: %d %s %+v", got.Code, got.Body.String(), stub.pageScope)
	}
	if got := get("/api/events/page?limit=invalid"); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid page limit accepted: %d", got.Code)
	}
	if got := get("/api/events/overview"); got.Code != http.StatusOK ||
		strings.Contains(got.Body.String(), scopeAssetB) ||
		len(stub.overviewScope) != 1 || stub.overviewScope[0] != scopeAssetA {
		t.Fatalf("event overview scope missing: %d %s %+v", got.Code, got.Body.String(), stub.overviewScope)
	}
	if got := get("/api/assets/page?limit=20"); got.Code != http.StatusOK ||
		strings.Contains(got.Body.String(), scopeAssetB) ||
		len(stub.assetPageScope) != 1 || stub.assetPageScope[0] != scopeAssetA {
		t.Fatalf("asset page scope missing: %d %s %+v", got.Code, got.Body.String(), stub.assetPageScope)
	}
	if got := get("/api/assets/page?runtime=bad"); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid asset runtime accepted: %d", got.Code)
	}
	if got := get("/api/notifications"); got.Code != http.StatusOK || strings.Contains(got.Body.String(), "private") {
		t.Fatalf("notification leaked: %d %s", got.Code, got.Body.String())
	}
	for _, path := range []string{
		"/api/assets/" + scopeAssetB, "/api/assets/" + scopeAssetB + "/samples",
		"/api/assets/" + scopeAssetB + "/wait-history", "/api/assets/" + scopeAssetB + "/monitoring-context",
		"/api/events/" + scopeEventB, "/api/events/" + scopeEventB + "/report",
		"/api/inspection-runs/" + scopeEventB + "/report"} {
		if got := get(path); got.Code != http.StatusNotFound {
			t.Fatalf("other asset accessible through %s: %d %s", path, got.Code, got.Body.String())
		}
	}
	if got := get("/api/storage"); got.Code != http.StatusForbidden {
		t.Fatalf("cross-asset storage aggregate accessible: %d", got.Code)
	}
	if got := get("/api/alert-rules"); got.Code != http.StatusOK {
		t.Fatalf("shared alert definitions unavailable: %d", got.Code)
	}
	var filtered map[string][]json.RawMessage
	if err := json.Unmarshal(get("/api/assets").Body.Bytes(), &filtered); err != nil || len(filtered["assets"]) != 1 {
		t.Fatal("asset whitelist not applied", err)
	}
}

func TestScopedCollectionRejectsMissingAssetIdentity(t *testing.T) {
	_, err := filterScopedCollection("/api/notifications", map[string]any{
		"notifications": []any{map[string]any{"lastError": "secret"}}}, map[string]bool{scopeAssetA: true})
	if err == nil {
		t.Fatal("unowned notification was not rejected")
	}
	_, err = filterScopedCollection("/api/storage", map[string]any{"storage": map[string]any{}},
		map[string]bool{scopeAssetA: true})
	if err == nil {
		t.Fatal("global storage summary was not rejected")
	}
}

func TestScopedViewerAllowsOnlyOwnedDirectReads(t *testing.T) {
	stub := &scopedViewerStub{}
	allowed := map[string]bool{scopeAssetA: true}
	for _, path := range []string{
		"/api/assets/" + scopeAssetA, "/api/assets/" + scopeAssetA + "/capacity",
		"/api/events/" + scopeEventA, "/api/events/" + scopeEventA + "/report"} {
		w := httptest.NewRecorder()
		if !authorizeScopedViewerRead(w, httptest.NewRequest(http.MethodGet, path, nil), stub, allowed) {
			t.Fatalf("owned resource was denied: %s: %d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	if authorizeScopedViewerRead(w, httptest.NewRequest(http.MethodGet, "/api/unscoped-future-endpoint", nil),
		stub, allowed) || w.Code != http.StatusForbidden {
		t.Fatal("unknown endpoint did not fail closed")
	}
}
