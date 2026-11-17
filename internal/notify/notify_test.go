package notify

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cuipengdba/dbsphere/internal/store"
)

func TestWebhookSignedDeliveryAndSeverity(t *testing.T) {
	var delivered int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivered++
		body, _ := io.ReadAll(r.Body)
		mac := hmac.New(sha256.New, []byte("test-secret"))
		_, _ = mac.Write(body)
		if r.Method != http.MethodPost || r.Header.Get("X-DbSphere-Delivery-Id") != "delivery-id" ||
			r.Header.Get("X-DbSphere-Signature") != "sha256="+hex.EncodeToString(mac.Sum(nil)) ||
			!strings.Contains(string(body), `"eventId":"incident-id"`) {
			t.Errorf("signed webhook payload mismatch: %s", body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv("DBSPHERE_WEBHOOK_URL", server.URL)
	t.Setenv("DBSPHERE_WEBHOOK_SECRET", "test-secret")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Client = server.Client()
	item := store.Notification{ID: "delivery-id", Channel: "webhook", EventID: "incident-id",
		ObservedAt: time.Now(), Payload: json.RawMessage(`{"severity":"critical","ruleId":"pg_lock_wait"}`)}
	if outcome, err := cfg.Send(context.Background(), item); err != nil || outcome != "delivered" || delivered != 1 {
		t.Fatalf("webhook delivery failed: %s %v", outcome, err)
	}
	item.Payload = json.RawMessage(`{"severity":"info"}`)
	if outcome, err := cfg.Send(context.Background(), item); err != nil || outcome != "skipped" || delivered != 1 {
		t.Fatalf("severity routing failed: %s %v", outcome, err)
	}
}

func TestNotificationConfigRejectsInsecureWebhook(t *testing.T) {
	t.Setenv("DBSPHERE_WEBHOOK_URL", "http://example.com/hook")
	if _, err := Load(); err == nil {
		t.Fatal("insecure external webhook accepted")
	}
}

func TestSMTPPlainLoopbackDelivery(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	received := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, "220 local ESMTP\r\n")
		var body strings.Builder
		data := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if data {
				if line == "." {
					data = false
					_, _ = io.WriteString(conn, "250 accepted\r\n")
					received <- body.String()
				} else {
					body.WriteString(line + "\n")
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO") || strings.HasPrefix(line, "HELO"):
				_, _ = io.WriteString(conn, "250 local\r\n")
			case strings.HasPrefix(line, "MAIL FROM:") || strings.HasPrefix(line, "RCPT TO:"):
				_, _ = io.WriteString(conn, "250 ok\r\n")
			case line == "DATA":
				data = true
				_, _ = io.WriteString(conn, "354 send data\r\n")
			case line == "QUIT":
				_, _ = io.WriteString(conn, "221 bye\r\n")
				return
			default:
				_, _ = io.WriteString(conn, "500 unknown\r\n")
			}
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	t.Setenv("DBSPHERE_SMTP_HOST", "127.0.0.1")
	t.Setenv("DBSPHERE_SMTP_PORT", port)
	t.Setenv("DBSPHERE_SMTP_FROM", "DbSphere <alerts@example.test>")
	t.Setenv("DBSPHERE_SMTP_TO", "Operator <ops@example.test>")
	t.Setenv("DBSPHERE_SMTP_ALLOW_PLAIN", "true")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	item := store.Notification{Channel: "email", EventID: "incident-id", AssetName: "PG cluster",
		EventTitle: "Lock wait", ObservedAt: time.Now(),
		Payload: json.RawMessage(`{"severity":"critical","ruleId":"pg_lock_wait","source":"PostgreSQL"}`)}
	if outcome, err := cfg.Send(context.Background(), item); err != nil || outcome != "delivered" {
		t.Fatalf("SMTP send failed: %s %v", outcome, err)
	}
	select {
	case body := <-received:
		if !strings.Contains(body, "incident-id") || !strings.Contains(body, "pg_lock_wait") {
			t.Fatalf("SMTP body missing incident evidence: %s", body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SMTP test did not receive a message")
	}
}
