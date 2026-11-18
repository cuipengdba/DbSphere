package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAIConfigurationRequiresExplicitRemoteOptIn(t *testing.T) {
	if client, err := New(Config{}); err != nil || client != nil {
		t.Fatal("AI should be disabled by default")
	}
	for _, cfg := range []Config{
		{URL: "http://example.com/v1/chat/completions", Model: "test", AllowRemote: true},
		{URL: "https://example.com/v1/chat/completions", Model: "test"},
		{URL: "http://127.0.0.1:8080/v1/chat/completions", Model: ""},
		{URL: "http://127.0.0.1:8080/v1/other", Model: "test"},
	} {
		if _, err := New(cfg); !errors.Is(err, ErrInvalidConfiguration) {
			t.Fatalf("unsafe AI config accepted: %+v", cfg)
		}
	}
	if client, err := New(Config{URL: "https://example.com/v1/chat/completions",
		Model: "test", AllowRemote: true}); err != nil || client == nil {
		t.Fatal("explicit HTTPS AI endpoint was rejected")
	}
}

func TestAIRejectsInventedEvidenceAndKeepsProviderSecretOutOfResult(t *testing.T) {
	content := `{"summary":"数据库连接异常，需继续确认。","evidenceIds":["evidence-1"],"uncertainty":"缺少主机指标。","suggestedChecks":["检查 Pod 状态"]}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-secret" {
			t.Error("AI request did not use configured endpoint and token")
		}
		var input struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&input) != nil || input.Model != "test-model" ||
			len(input.Messages) != 2 || !strings.Contains(input.Messages[1].Content, "evidence-1") {
			t.Error("AI received an invalid evidence packet")
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":` +
			strconvQuote(content) + `}}]}`))
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL + "/v1/chat/completions", Model: "test-model",
		APIKey: "test-secret"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Analyze(context.Background(), "事件证据 evidence-1", []string{"evidence-1"})
	if err != nil || result.Model != "test-model" || len(result.EvidenceIDs) != 1 ||
		strings.Contains(result.Summary, "test-secret") {
		t.Fatalf("valid AI result rejected or secret leaked: %+v %v", result, err)
	}
	_, err = client.Analyze(context.Background(), "事件证据 evidence-1", []string{"other-evidence"})
	if !errors.Is(err, ErrInvalidEvidence) {
		t.Fatalf("invented AI evidence was accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Analyze(ctx, "事件证据 evidence-1", []string{"evidence-1"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("cancelled AI call did not fail without affecting rule diagnosis: %v", err)
	}
}

func strconvQuote(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestAIProviderTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()
	client, err := New(Config{URL: server.URL + "/v1/chat/completions", Model: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err = client.Analyze(ctx, "evidence-1", []string{"evidence-1"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("AI timeout did not fail safely: %v", err)
	}
}
