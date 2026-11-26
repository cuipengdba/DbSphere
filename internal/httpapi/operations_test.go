package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/assets"
	"github.com/cuipengdba/dbsphere/internal/collectorstate"
	"github.com/cuipengdba/dbsphere/internal/notify"
	"github.com/cuipengdba/dbsphere/internal/store"
	"github.com/jackc/pgx/v5"
)

func TestPG18OperationsWebhookRetryAndExternalIncident(t *testing.T) {
	dsn := os.Getenv("DBSPHERE_TEST_OPS_PG18_URL")
	if dsn == "" {
		t.Skip("PostgreSQL 18 is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	suffix := time.Now().UTC().Format("150405.000000000")
	a, err := db.CreateOrganization(ctx, "Operations QA A "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	b, err := db.CreateOrganization(ctx, "Operations QA B "+suffix)
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("01234567890123456789012345678901")
	makeAsset := func(name, organization string) string {
		t.Helper()
		raw, err := db.CreateAsset(ctx, assets.Input{Name: name, OrganizationID: organization, Host: "127.0.0.1", Port: 1,
			Database: "postgres", Username: "monitor", Password: "fixture-password", SSLMode: "disable", Runtime: "linux"}, key)
		if err != nil {
			t.Fatal(err)
		}
		var result struct{ ID string }
		if err := json.Unmarshal(raw, &result); err != nil || result.ID == "" {
			t.Fatalf("missing asset ID: %v", err)
		}
		return result.ID
	}
	asset, foreignAsset := makeAsset("Local Operations Asset", a.ID), makeAsset("Foreign Operations Asset", b.ID)
	defer db.DeleteAsset(context.Background(), asset)
	defer db.DeleteAsset(context.Background(), foreignAsset)
	db.ConfigureOperationsWebhook(a.ID)
	const opsToken = "integration-qa-fixture-token"
	const metricsToken = "metrics-qa-fixture-distinct-token"
	tracker := collectorstate.New(true, time.Minute, 2, time.Now())
	tracker.Begin(time.Now().Add(-2 * time.Second))
	tracker.Finish(time.Now(), collectorstate.Cycle{TargetFailures: 1})
	handler := New(db, AuthConfig{MasterKey: key, Operations: OperationsConfig{Token: opsToken, Organization: a.ID, MetricsToken: metricsToken}, CollectorState: tracker})
	call := func(method, path, token string, body any, status int) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(method, path, strings.NewReader(string(raw))).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, status, w.Body.String())
		}
		return w
	}
	call("GET", "/api/v1/assets", metricsToken, nil, 401)
	call("GET", "/api/v1/assets/"+foreignAsset, opsToken, nil, 404)
	page := call("GET", "/api/v1/assets?limit=1", opsToken, nil, 200).Body.String()
	if !strings.Contains(page, asset) || strings.Contains(page, foreignAsset) {
		t.Fatal("asset page crossed an organization boundary")
	}
	for _, id := range []string{asset, foreignAsset} {
		if result, err := db.CheckAsset(ctx, id, key); err != nil || result.SampleID == "" || result.Status != "error" {
			t.Fatalf("failed target did not produce a source sample: %+v %v", result, err)
		}
	}

	type delivery struct {
		DeliveryID, EventID, AssetID, OrganizationID string
	}
	var lock sync.Mutex
	var legacy, scoped []delivery
	secret1, secret2 := []byte("legacy-qa-signature-secret"), []byte("scoped-qa-signature-secret")
	receiver := func(secret []byte, received *[]delivery, reply int, retryAfter string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			data, err := io.ReadAll(r.Body)
			mac := hmac.New(sha256.New, secret)
			_, _ = mac.Write(data)
			var item delivery
			if err != nil || json.Unmarshal(data, &item) != nil || item.DeliveryID == "" ||
				item.DeliveryID != r.Header.Get("Idempotency-Key") || item.DeliveryID != r.Header.Get("X-DbSphere-Delivery-Id") ||
				!hmac.Equal([]byte(r.Header.Get("X-DbSphere-Signature")), []byte("sha256="+hex.EncodeToString(mac.Sum(nil)))) {
				t.Error("invalid delivery identity or signature")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			lock.Lock()
			previous := false
			for _, old := range *received {
				previous = previous || old.DeliveryID == item.DeliveryID
			}
			*received = append(*received, item)
			lock.Unlock()
			if !previous {
				w.Header().Set("Retry-After", retryAfter)
				w.WriteHeader(reply)
				return
			}
			w.WriteHeader(http.StatusAccepted)
		}))
	}
	first := receiver(secret1, &legacy, 429, "120")
	second := receiver(secret2, &scoped, 503, time.Now().Add(5*time.Minute).UTC().Format(http.TimeFormat))
	defer first.Close()
	defer second.Close()
	firstURL, _ := url.Parse(first.URL)
	secondURL, _ := url.Parse(second.URL)
	config := notify.Config{WebhookURL: firstURL, Secret: secret1, WebhookMin: "warning", EmailMin: "warning",
		OperationsWebhookURL: secondURL, OperationsWebhookSecret: secret2, OperationsWebhookOrganization: a.ID,
		Client: &http.Client{Timeout: 2 * time.Second}}
	if count, err := db.DispatchDueNotifications(ctx, 50, config.Send); err != nil || count < 5 {
		t.Fatalf("dispatch did not process both webhook routes: count=%d error=%v", count, err)
	}
	lock.Lock()
	initialLegacy, initialScoped := append([]delivery{}, legacy...), append([]delivery{}, scoped...)
	lock.Unlock()
	if len(initialLegacy) < 2 || len(initialScoped) == 0 {
		t.Fatalf("missing deliveries: legacy=%d scoped=%d", len(initialLegacy), len(initialScoped))
	}
	for _, item := range initialScoped {
		if item.AssetID != asset || item.OrganizationID != a.ID {
			t.Fatalf("scoped webhook delivered a foreign asset: %+v", item)
		}
	}
	for _, route := range []struct {
		batch   []delivery
		minimum int
	}{{initialLegacy, 85}, {initialScoped, 250}} {
		for _, item := range route.batch {
			var seconds int
			if err := conn.QueryRow(ctx, `SELECT extract(epoch FROM next_attempt_at-now())::int
				FROM notification_outbox WHERE id=$1 AND status='pending' AND attempts=1`, item.DeliveryID).Scan(&seconds); err != nil || seconds < route.minimum {
				t.Fatalf("retry delay missing: seconds=%d error=%v", seconds, err)
			}
			if _, err := conn.Exec(ctx, "UPDATE notification_outbox SET next_attempt_at=now()-interval '1 second' WHERE id=$1", item.DeliveryID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := db.DispatchDueNotifications(ctx, 50, config.Send); err != nil {
		t.Fatal(err)
	}
	for _, route := range [][]delivery{initialLegacy, initialScoped} {
		for _, item := range route {
			var status string
			if err := conn.QueryRow(ctx, "SELECT status FROM notification_outbox WHERE id=$1", item.DeliveryID).Scan(&status); err != nil || status != "delivered" {
				t.Fatalf("receiver retry did not complete for stable ID %s: %q %v", item.DeliveryID, status, err)
			}
		}
	}
	original := initialScoped[0]
	post := func(input store.ExternalIncidentInput, status int) store.ExternalIncidentResult {
		t.Helper()
		w := call("POST", "/api/v1/external-incidents", opsToken, input, status)
		if status >= 400 {
			return store.ExternalIncidentResult{}
		}
		var result struct{ Data store.ExternalIncidentResult }
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Data
	}
	ticket := store.ExternalIncidentInput{Source: "test-ticket", ExternalID: "DB-41", Kind: "ticket", State: "linked",
		AssetID: asset, EventID: original.EventID, Title: "Database operations test", URL: "https://tickets.example.org/DB-41"}
	linked := post(ticket, 201)
	repeated := post(ticket, 200)
	if linked.EventID != original.EventID || !repeated.Duplicate || linked.EvidenceID != repeated.EvidenceID {
		t.Fatalf("ticket did not return the original event: %+v %+v", linked, repeated)
	}
	receipt := store.TicketReceipt{Source: ticket.Source, ExternalID: ticket.ExternalID, ReceiptID: "status-change-1",
		DeliveryID: original.DeliveryID, AssetID: asset, EventID: original.EventID, State: "in_progress", ExternalOwner: "support-desk"}
	const receiptsPath = "/api/v1/ticket-receipts"
	call("POST", receiptsPath, metricsToken, receipt, 401)
	accepted := call("POST", receiptsPath, opsToken, receipt, 201)
	duplicate := call("POST", receiptsPath, opsToken, receipt, 200)
	var recorded, deduped struct{ Data store.ExternalIncidentResult }
	if err := json.Unmarshal(accepted.Body.Bytes(), &recorded); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(duplicate.Body.Bytes(), &deduped); err != nil || !deduped.Data.Duplicate ||
		recorded.Data.EvidenceID == "" || deduped.Data.EvidenceID != recorded.Data.EvidenceID || recorded.Data.EventID != original.EventID {
		t.Fatalf("receipt did not preserve one incident and evidence ID: %+v %+v %v", recorded, deduped, err)
	}
	receipt.State = "resolved"
	call("POST", receiptsPath, opsToken, receipt, 409)
	receipt.ReceiptID = "status-change-2"
	receipt.LocalOwner = "unmapped-operator"
	call("POST", receiptsPath, opsToken, receipt, 404)
	receipt.LocalOwner = ""
	call("POST", receiptsPath, opsToken, receipt, 201)
	external := store.ExternalIncidentInput{Source: "prometheus", ExternalID: "external-episode-1", Kind: "alert",
		State: "firing", AssetID: asset, Title: "External alert fixture"}
	opened := post(external, 201)
	external.State = "resolved"
	closed := post(external, 201)
	if opened.EventID == "" || closed.EventID != opened.EventID || closed.State != "resolved" || !post(external, 200).Duplicate {
		t.Fatalf("external recovery changed event identity: %+v %+v", opened, closed)
	}
	external.AssetID = foreignAsset
	post(external, 404)
	event := call("GET", "/api/v1/events/"+linked.EventID, opsToken, nil, 200).Body.String()
	if !strings.Contains(event, "DB-41") || !strings.Contains(event, linked.EvidenceID) || !strings.Contains(event, "/#incidents?event="+original.EventID) ||
		!strings.Contains(event, recorded.Data.EvidenceID) || !strings.Contains(event, original.DeliveryID) {
		t.Fatal("event lookup omitted the attached receipt evidence")
	}
	call("GET", "/metrics", opsToken, nil, 401)
	metric := call("GET", "/metrics", metricsToken, nil, 200)
	if !strings.HasPrefix(metric.Header().Get("Content-Type"), "text/plain; version=0.0.4") ||
		!strings.Contains(metric.Body.String(), "dbsphere_store_up 1") || !strings.Contains(metric.Body.String(), "dbsphere_collector_cycles_total 1") {
		t.Fatal("platform metric scrape failed")
	}
	t.Logf("legacy=%d scoped=%d event=%s ticketEvidence=%s recoveredExternal=%s", len(initialLegacy), len(initialScoped), original.EventID, linked.EvidenceID, closed.EventID)
}
