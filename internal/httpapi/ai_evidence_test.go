package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/ai"
	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/security"
	"github.com/cuipengdba/dbsphere/internal/store"
	"github.com/jackc/pgx/v5"
)

func TestPG18AIEvidenceReviewModesAndOrgBoundary(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_AI_EVIDENCE_PG18_URL")
	if dsn == "" {
		t.Skip("isolated PostgreSQL 18 evidence database is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	pg, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pg.Close()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(context.Background())
	organizationA, err := pg.CreateOrganization(ctx, "Evidence tenant A")
	if err != nil {
		t.Fatal(err)
	}
	organizationB, err := pg.CreateOrganization(ctx, "Evidence tenant B")
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("01234567890123456789012345678901")
	makeEvent := func(org string) (string, string, string) {
		t.Helper()
		raw, err := pg.CreateAsset(ctx, assets.Input{OrganizationID: org, Name: "PRIVATE-ASSET-NAME",
			Host: "127.0.0.1", Port: 1, Database: "postgres", Username: "monitor", Password: "PRIVATE-DB-CREDENTIAL",
			SSLMode: "disable", Runtime: "linux"}, key)
		var asset struct{ ID string }
		if err != nil || json.Unmarshal(raw, &asset) != nil || asset.ID == "" {
			t.Fatalf("asset fixture unavailable: %v", err)
		}
		result, err := pg.CheckAsset(ctx, asset.ID, key)
		if err != nil || result.SampleID == "" {
			t.Fatalf("database sample unavailable: %v", err)
		}
		var event string
		if err := db.QueryRow(ctx, `SELECT id::text FROM event_keys WHERE asset_id=$1 ORDER BY started_at DESC LIMIT 1`, asset.ID).Scan(&event); err != nil {
			t.Fatal(err)
		}
		var evidence string
		if err := db.QueryRow(ctx, `INSERT INTO evidence(id,event_id,kind,observed_at,payload_json)
			VALUES(gen_random_uuid(),$1,'collection_error',now(),jsonb_build_object(
				'sampleId',$2::text,'value',42,'note','PRIVATE-PROMPT-PAYLOAD','sql','PRIVATE-SQL-TEXT'))
			RETURNING id::text`, event, result.SampleID).Scan(&evidence); err != nil {
			t.Fatal(err)
		}
		return event, evidence, asset.ID
	}
	local, known, asset := makeEvent(organizationA.ID)
	foreign, foreignEvidence, foreignAsset := makeEvent(organizationB.ID)
	defer pg.DeleteAsset(context.Background(), asset)
	defer pg.DeleteAsset(context.Background(), foreignAsset)
	var empty string
	if err := db.QueryRow(ctx, "INSERT INTO event_keys(id,asset_id,started_at) VALUES(gen_random_uuid(),$1,now()) RETURNING id::text", asset).Scan(&empty); err != nil {
		t.Fatal(err)
	}
	const password = "ai-review-root-fixture"
	hash, err := security.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	build := func(client *ai.Client) http.Handler {
		return New(pg, AuthConfig{MasterKey: key, PasswordHash: hash, AI: client, AIDailyLimit: 3})
	}
	type session struct {
		Cookie *http.Cookie
		CSRF   string
	}
	call := func(handler http.Handler, auth session, method, path string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		request := httptest.NewRequest(method, path, strings.NewReader(string(raw))).WithContext(ctx)
		request.Header.Set("Content-Type", "application/json")
		if auth.Cookie != nil {
			request.AddCookie(auth.Cookie)
			request.Header.Set("X-CSRF-Token", auth.CSRF)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, request)
		if w.Code != status {
			t.Fatalf("%s %s: expected %d, got %d: %s", method, path, status, w.Code, w.Body.String())
		}
		return w
	}
	login := func(handler http.Handler, user, pass string) session {
		t.Helper()
		w := call(handler, session{}, "POST", "/api/login", map[string]string{"username": user, "password": pass}, 200)
		var auth session
		if err := json.Unmarshal(w.Body.Bytes(), &auth); err != nil {
			t.Fatal(err)
		}
		auth.Cookie = w.Result().Cookies()[0]
		return auth
	}
	noModel := build(nil)
	root := login(noModel, "", password)
	route := func(id string) string { return "/api/events/" + id + "/ai-evidence-review" }
	read := func(handler http.Handler, auth session, method, event, want string) map[string]any {
		t.Helper()
		w := call(handler, auth, method, route(event), nil, 200)
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["eventId"] != event || result["status"] != want {
			t.Fatalf("review mode or event mismatch: %v %s", err, w.Body.String())
		}
		return result
	}
	_ = read(noModel, root, "GET", local, "no_model")
	_ = read(noModel, root, "GET", empty, "insufficient")
	call(noModel, root, "POST", "/api/admin/accounts", map[string]any{"username": "evidence-admin-a", "password": "evidence-password-a",
		"role": "admin", "organizationId": organizationA.ID}, 201)
	adminA := login(noModel, "evidence-admin-a", "evidence-password-a")
	call(noModel, adminA, "GET", route(foreign), nil, 404)

	var requests atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := requests.Add(1)
		var body struct {
			Messages       []struct{ Role, Content string }
			ResponseFormat json.RawMessage `json:"response_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) != 2 || len(body.ResponseFormat) == 0 {
			t.Error("model did not receive bounded schema and source envelope")
			w.WriteHeader(400)
			return
		}
		sourceText := body.Messages[1].Content
		for _, value := range []string{"PRIVATE-ASSET-NAME", "PRIVATE-DB-CREDENTIAL", "PRIVATE-PROMPT-PAYLOAD", "PRIVATE-SQL-TEXT",
			foreign, foreignEvidence, foreignAsset} {
			if strings.Contains(sourceText, value) {
				t.Error("private or foreign text sent to model")
			}
		}
		if !strings.Contains(sourceText, known) || !strings.Contains(sourceText, `"value":42`) {
			t.Error("allowed numeric evidence omitted")
		}
		if turn == 2 {
			<-r.Context().Done()
			return
		}
		cite := known
		if turn == 3 {
			cite = foreignEvidence
		}
		report := ai.EvidenceReview{Uncertainty: "已有观测，需要人工核对。", Claims: []ai.ReviewClaim{
			{Kind: "candidate", Text: "采集异常可能与连接路径有关。", EvidenceIDs: []string{cite}},
			{Kind: "counterevidence", Text: "当前样本只证明采集失败，证据范围有限。", EvidenceIDs: []string{cite}},
			{Kind: "verification", Text: "结合原始采样核对故障时点。", EvidenceIDs: []string{cite}},
		}}
		data, _ := json.Marshal(report)
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{
			map[string]any{"finish_reason": "stop", "message": map[string]string{"content": string(data)}},
		}})
	}))
	defer model.Close()
	client, err := ai.New(ai.Config{URL: model.URL + "/v1/chat/completions", Model: "review-qa", ReviewSchema: true, ReviewTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	withModel := build(client)
	_ = read(withModel, adminA, "POST", empty, "insufficient")
	accepted := read(withModel, adminA, "POST", local, "model")
	_ = read(withModel, adminA, "GET", local, "model_saved")
	_ = read(withModel, adminA, "POST", local, "model_unavailable")
	_ = read(withModel, adminA, "POST", local, "invalid_citations")
	_ = read(withModel, adminA, "POST", local, "budget_exhausted")
	call(withModel, adminA, "POST", route(foreign), nil, 404)
	if requests.Load() != 3 {
		t.Fatalf("budget or scope leaked a model call: %d", requests.Load())
	}
	var saved, failed, pending int
	if err := db.QueryRow(ctx, `SELECT count(*) FILTER (WHERE kind='ai_evidence_review'),
		count(*) FILTER (WHERE kind='ai_invocation' AND payload_json->>'status'='failed'),
		count(*) FILTER (WHERE kind='ai_invocation' AND payload_json->>'status'='pending')
		FROM evidence WHERE event_id=$1`, local).Scan(&saved, &failed, &pending); err != nil || saved != 1 || failed != 2 || pending != 0 {
		t.Fatalf("the result or budget audit was lost: saved=%d failed=%d pending=%d %v", saved, failed, pending, err)
	}
	t.Logf("event=%s evidence=%s invocation=%v modelCalls=%d saved=%d failed=%d orgScoped=true",
		local, known, accepted["invocationId"], requests.Load(), saved, failed)
}
