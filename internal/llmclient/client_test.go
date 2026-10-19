package llmclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/secrets"
	"github.com/earlgrey/piproxy/internal/upstream"
)

func newTestSetup(t *testing.T, providerType providers.ProviderType, model string, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	registry := providers.NewRegistry()
	credRef := "providers/test/api-key"
	if err := registry.RegisterProvider(&providers.Provider{
		Type:            providerType,
		Name:            "test",
		UpstreamBaseURL: srv.URL,
		CredentialID:    credRef,
		Models: []providers.Model{{
			PublicID:   model,
			UpstreamID: model,
			Name:       model,
		}},
		Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	os.Setenv("LLMTEST_PROVIDERS_TEST_API_KEY", "sk-internal")
	t.Cleanup(func() { os.Unsetenv("LLMTEST_PROVIDERS_TEST_API_KEY") })
	creds := secrets.NewCredentialStore(secrets.NewEnvStore("LLMTEST_"))

	up := upstream.NewClient(upstream.Config{Timeout: 5 * time.Second, MaxConnsPerHost: 2, MaxIdleConns: 2})
	return New(registry, creds, up)
}

func TestCompleteAnthropic(t *testing.T) {
	var gotBody map[string]any
	var gotAPIKey string
	c := newTestSetup(t, providers.ProviderAnthropic, "claude-haiku-4-5-20251001", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAPIKey = r.Header.Get("x-api-key")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{{"type": "text", "text": "distilled output"}},
		})
	})

	text, err := c.Complete(context.Background(), Request{
		Model:  "claude-haiku-4-5-20251001",
		System: "You distill lessons.",
		User:   "transcript here",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if text != "distilled output" {
		t.Errorf("text = %q", text)
	}
	if gotAPIKey != "sk-internal" {
		t.Errorf("x-api-key = %q", gotAPIKey)
	}
	if gotBody["system"] != "You distill lessons." {
		t.Errorf("system = %v", gotBody["system"])
	}
	if gotBody["model"] != "claude-haiku-4-5-20251001" {
		t.Errorf("model = %v", gotBody["model"])
	}
}

func TestCompleteOpenAICompatible(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	c := newTestSetup(t, providers.ProviderDeepSeek, "deepseek-v4-flash", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "compacted"}}},
		})
	})

	text, err := c.Complete(context.Background(), Request{
		Model:  "deepseek-v4-flash",
		System: "Compact this.",
		User:   "memory body",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if text != "compacted" {
		t.Errorf("text = %q", text)
	}
	if gotAuth != "Bearer sk-internal" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	msgs, _ := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Errorf("messages = %v", gotBody["messages"])
	}
}

func TestCompleteOpenAICompatibleReasoningContentFallback(t *testing.T) {
	c := newTestSetup(t, providers.ProviderDeepSeek, "deepseek-v4-flash", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{
					"content":           "",
					"reasoning_content": `{"observations":[{"kind":"lesson","content":"hook-driven memory works"}]}`,
				},
			}},
		})
	})

	text, err := c.Complete(context.Background(), Request{
		Model: "deepseek-v4-flash",
		User:  "distill",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if text != `{"observations":[{"kind":"lesson","content":"hook-driven memory works"}]}` {
		t.Errorf("text = %q", text)
	}
}

func TestCompleteUpstreamError(t *testing.T) {
	c := newTestSetup(t, providers.ProviderDeepSeek, "deepseek-v4-flash", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"rate limited"}`, http.StatusTooManyRequests)
	})
	_, err := c.Complete(context.Background(), Request{Model: "deepseek-v4-flash", User: "x"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestCompleteUnknownModel(t *testing.T) {
	c := newTestSetup(t, providers.ProviderDeepSeek, "deepseek-v4-flash", func(w http.ResponseWriter, r *http.Request) {})
	_, err := c.Complete(context.Background(), Request{Model: "unknown-model-xyz", User: "x"})
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}
