package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/cuipengdba/dbsphere/internal/store"
)

func TestPG18WorkOrderHTTPSeparatesRequesterApproverAndExecutor(t *testing.T) {
	url := os.Getenv("DBSPHERE_TEST_PG18_URL")
	if url == "" {
		t.Skip("PostgreSQL 18 store is required")
	}
	ctx := context.Background()
	pg, err := store.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	key := []byte("01234567890123456789012345678901")
	created, err := pg.CreateAsset(ctx, assets.Input{Name: "HTTP work order", Host: "127.0.0.1",
		Port: 1, Database: "postgres", Username: "postgres", Password: "test-secret",
		SSLMode: "disable", Runtime: "linux"}, key)
	if err != nil {
		t.Fatal(err)
	}
	var asset struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created, &asset); err != nil {
		t.Fatal(err)
	}
	defer pg.DeleteAsset(ctx, asset.ID)
	if _, err := pg.CheckAsset(ctx, asset.ID, key); err != nil {
		t.Fatal(err)
	}
	var eventID string
	// Fetch the event ID through the authenticated collection API below.
	adminHash, err := security.HashPassword("workorder-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	approverHash, err := security.HashPassword("workorder-approver-password")
	if err != nil {
		t.Fatal(err)
	}
	handler := New(pg, AuthConfig{MasterKey: key, PasswordHash: adminHash,
		ApproverPasswordHash: approverHash})
	login := func(password string) (*http.Cookie, string) {
		r := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"password":"`+password+`"}`))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("login failed: %d %s", w.Code, w.Body.String())
		}
		var data struct {
			CSRF string `json:"csrf"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
			t.Fatal(err)
		}
		return w.Result().Cookies()[0], data.CSRF
	}
	adminCookie, adminCSRF := login("workorder-admin-password")
	approverCookie, approverCSRF := login("workorder-approver-password")
	call := func(method, path, body string, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.AddCookie(cookie)
		if body != "" {
			r.Header.Set("Content-Type", "application/json")
		}
		if method != http.MethodGet {
			r.Header.Set("X-CSRF-Token", csrf)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	list := call(http.MethodGet, "/api/events", "", adminCookie, adminCSRF)
	if list.Code != http.StatusOK {
		t.Fatalf("event list: %d", list.Code)
	}
	var events struct {
		Events []struct {
			ID      string `json:"id"`
			AssetID string `json:"assetId"`
		} `json:"events"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &events); err != nil {
		t.Fatal(err)
	}
	for _, item := range events.Events {
		if item.AssetID == asset.ID {
			eventID = item.ID
			break
		}
	}
	if eventID == "" {
		t.Fatal("isolated event not found")
	}
	requestPath := "/api/events/" + eventID + "/work-orders"
	if got := call(http.MethodPost, requestPath, `{"reason":"复查数据库连接故障"}`, approverCookie, approverCSRF); got.Code != http.StatusForbidden {
		t.Fatal("approver created a request", got.Code)
	}
	requested := call(http.MethodPost, requestPath, `{"reason":"复查数据库连接故障"}`, adminCookie, adminCSRF)
	if requested.Code != http.StatusCreated {
		t.Fatalf("request failed: %d %s", requested.Code, requested.Body.String())
	}
	var order struct {
		Order struct {
			ID string `json:"id"`
		} `json:"order"`
	}
	if err := json.Unmarshal(requested.Body.Bytes(), &order); err != nil || order.Order.ID == "" {
		t.Fatal(err)
	}
	base := "/api/work-orders/" + order.Order.ID
	if got := call(http.MethodPost, base+"/approve", "", adminCookie, adminCSRF); got.Code != http.StatusForbidden {
		t.Fatal("requester self-approved", got.Code)
	}
	if got := call(http.MethodPost, base+"/execute", "", adminCookie, adminCSRF); got.Code != http.StatusConflict {
		t.Fatal("unapproved action executed", got.Code)
	}
	if got := call(http.MethodPost, base+"/execute", "", approverCookie, approverCSRF); got.Code != http.StatusForbidden {
		t.Fatal("approver executed action", got.Code)
	}
	if got := call(http.MethodPost, base+"/approve", "", approverCookie, approverCSRF); got.Code != http.StatusOK {
		t.Fatalf("separate approval failed: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodPost, base+"/execute", "", adminCookie, adminCSRF); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), `"sampleId"`) {
		t.Fatalf("approved collection failed: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodPost, base+"/execute", "", adminCookie, adminCSRF); got.Code != http.StatusConflict {
		t.Fatal("approved action ran twice", got.Code)
	}
	if got := call(http.MethodGet, requestPath, "", approverCookie, approverCSRF); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), order.Order.ID) {
		t.Fatal("approver cannot inspect order", got.Code)
	}
	if got := call(http.MethodGet, "/api/events/"+eventID+"/audit", "", adminCookie, adminCSRF); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"actor":"approver"`) ||
		!strings.Contains(got.Body.String(), base+"/execute") {
		t.Fatalf("event audit lost separate actor or execution: %d %s", got.Code, got.Body.String())
	}
	if got := call(http.MethodPost, "/api/assets", `{}`, approverCookie, approverCSRF); got.Code != http.StatusForbidden {
		t.Fatal("approver modified assets", got.Code)
	}
}
