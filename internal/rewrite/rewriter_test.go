package rewrite_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/rewrite"
)

func TestNewRequestRewriter(t *testing.T) {
	r := providers.NewRegistry()
	for _, p := range providers.DefaultProviders() {
		r.RegisterProvider(p)
	}

	rw := rewrite.NewRequestRewriter(r)
	if rw == nil {
		t.Fatal("expected non-nil rewriter")
	}
}

func TestRewriteOpenAI(t *testing.T) {
	r := providers.NewRegistry()
	provider := &providers.Provider{
		Type:            providers.ProviderOpenAI,
		PublicBasePath:  "/openai",
		UpstreamBaseURL: "https://api.openai.com",
		AllowedPaths:    []string{"/v1/chat/completions"},
		Enabled:         true,
	}
	r.RegisterProvider(provider)

	rw := rewrite.NewRequestRewriter(r)
	req, _ := http.NewRequest("POST", "http://proxy.test/openai/v1/chat/completions", nil)
	req.Header.Set("X-Request-ID", "req_123")

	result, err := rw.Rewrite(req, provider)
	if err != nil {
		t.Fatalf("Rewrite failed: %v", err)
	}

	if !strings.HasPrefix(result.UpstreamURL, "https://api.openai.com/v1/chat/completions") {
		t.Errorf("unexpected upstream URL: %s", result.UpstreamURL)
	}
	if result.UpstreamMethod != "POST" {
		t.Errorf("expected POST, got %s", result.UpstreamMethod)
	}
	if result.Headers.Get("X-Corp-Request-ID") != "req_123" {
		t.Errorf("expected X-Corp-Request-ID header")
	}
}

func TestRewriteZAICodingEndpointTrimsPublicVersionPrefix(t *testing.T) {
	r := providers.NewRegistry()
	provider := &providers.Provider{
		ID:              "prov_zai",
		Name:            "ZAI",
		Type:            providers.ProviderOpenAI,
		PublicBasePath:  "/openai/v1",
		UpstreamBaseURL: "https://api.z.ai/api/coding/paas/v4",
		AllowedPaths:    []string{"/chat/completions", "/models"},
		CredentialID:    "provider-credential:test",
		Enabled:         true,
	}
	r.RegisterProvider(provider)

	rw := rewrite.NewRequestRewriter(r)
	req, _ := http.NewRequest("POST", "http://proxy.test/openai/v1/chat/completions", nil)

	result, err := rw.Rewrite(req, provider)
	if err != nil {
		t.Fatalf("Rewrite failed: %v", err)
	}

	want := "https://api.z.ai/api/coding/paas/v4/chat/completions"
	if result.UpstreamURL != want {
		t.Fatalf("upstream URL = %q, want %q", result.UpstreamURL, want)
	}
}

func TestRewriteStripsCredentialHeaders(t *testing.T) {
	r := providers.NewRegistry()
	provider := &providers.Provider{
		Type:            providers.ProviderOpenAI,
		PublicBasePath:  "/openai",
		UpstreamBaseURL: "https://api.openai.com",
		AllowedPaths:    []string{"/v1/chat/completions"},
		Enabled:         true,
	}
	r.RegisterProvider(provider)

	rw := rewrite.NewRequestRewriter(r)
	req, _ := http.NewRequest("POST", "http://proxy.test/openai/v1/chat/completions", nil)

	// Headers that must never be forwarded to the upstream.
	leakyHeaders := []string{
		"Authorization",
		"Proxy-Authorization",
		"X-Corp-Proxy-Key",
		"X-Corp-Provider-ID",
		"X-Project-ID",
		"X-Extension-Version",
		"X-Api-Key",
		"Api-Key",
		"X-Auth-Token",
		"Cookie",
		"Set-Cookie",
	}
	for _, h := range leakyHeaders {
		req.Header.Set(h, "secret-"+h)
	}
	// A benign header that should be forwarded.
	req.Header.Set("Content-Type", "application/json")

	result, err := rw.Rewrite(req, provider)
	if err != nil {
		t.Fatalf("Rewrite failed: %v", err)
	}

	for _, h := range leakyHeaders {
		if v := result.Headers.Get(h); v != "" {
			t.Errorf("header %q leaked upstream: %q", h, v)
		}
	}
	if result.Headers.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type to be forwarded")
	}
}

func TestRewriteDisallowedPath(t *testing.T) {
	r := providers.NewRegistry()
	provider := &providers.Provider{
		Type:            providers.ProviderOpenAI,
		PublicBasePath:  "/openai",
		UpstreamBaseURL: "https://api.openai.com",
		AllowedPaths:    []string{"/v1/chat/completions"},
		Enabled:         true,
	}
	r.RegisterProvider(provider)

	rw := rewrite.NewRequestRewriter(r)
	req, _ := http.NewRequest("GET", "http://proxy.test/openai/v1/other", nil)

	_, err := rw.Rewrite(req, provider)
	if err == nil {
		t.Error("expected error for disallowed path")
	}
}

func TestRewriteRejectsAllowedPathPrefixCollision(t *testing.T) {
	r := providers.NewRegistry()
	provider := &providers.Provider{
		Type:            providers.ProviderOpenAI,
		PublicBasePath:  "/openai",
		UpstreamBaseURL: "https://api.openai.com",
		AllowedPaths:    []string{"/v1/models"},
		Enabled:         true,
	}
	r.RegisterProvider(provider)

	rw := rewrite.NewRequestRewriter(r)
	req, _ := http.NewRequest("GET", "http://proxy.test/openai/v1/models-evil", nil)

	_, err := rw.Rewrite(req, provider)
	if err == nil {
		t.Error("expected error for path prefix collision")
	}
}

func TestValidateEgress(t *testing.T) {
	err := rewrite.ValidateEgress("https://api.openai.com/v1/chat", []string{"api.openai.com"})
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	err = rewrite.ValidateEgress("https://evil.com/steal", []string{"api.openai.com"})
	if err == nil {
		t.Error("expected error for disallowed egress")
	}

	err = rewrite.ValidateEgress("http://api.openai.com/v1/chat", []string{"api.openai.com"})
	if err == nil {
		t.Error("expected error for non-HTTPS egress")
	}

	err = rewrite.ValidateEgress("https://127.0.0.1/v1/chat", []string{"127.0.0.1"})
	if err == nil {
		t.Error("expected error for private egress host")
	}

	err = rewrite.ValidateEgressWithOptions("http://127.0.0.1/v1/chat", []string{"127.0.0.1"}, true)
	if err != nil {
		t.Errorf("expected private egress override to allow local test URL, got: %v", err)
	}
}

func TestContextOverflowNormalized(t *testing.T) {
	// OpenAI overflow patterns.
	patterns := []struct {
		body      string
		provider  providers.ProviderType
		expectHit bool
	}{
		{`{"error":{"message":"This model's maximum context length is 8192 tokens"}}`, providers.ProviderOpenAI, true},
		{`{"error":{"message":"normal error"}}`, providers.ProviderOpenAI, false},
		{`{"type":"error","error":{"type":"prompt_too_long"}}`, providers.ProviderAnthropic, false},
	}

	for _, p := range patterns {
		hit, normalized := rewrite.ContextOverflowNormalized(p.provider, []byte(p.body))
		if p.expectHit && !hit {
			t.Errorf("expected overflow hit for: %s", p.body)
		}
		if p.expectHit && normalized != "context_length_exceeded" {
			t.Errorf("expected 'context_length_exceeded', got %q", normalized)
		}
		if !p.expectHit && hit {
			t.Errorf("unexpected overflow hit for: %s", p.body)
		}
	}
}

func TestInjectCredential(t *testing.T) {
	tests := []struct {
		apiType    providers.ProviderType
		credential string
		headerKey  string
		headerVal  string
	}{
		{providers.ProviderOpenAI, "sk-test123", "Authorization", "Bearer sk-test123"},
		{providers.ProviderAnthropic, "sk-ant-test", "x-api-key", "sk-ant-test"},
	}

	for _, tt := range tests {
		req, _ := http.NewRequest("POST", "http://test", nil)
		rewrite.InjectCredential(req, tt.apiType, tt.credential)
		if req.Header.Get(tt.headerKey) != tt.headerVal {
			t.Errorf("expected header %s = %q, got %q", tt.headerKey, tt.headerVal, req.Header.Get(tt.headerKey))
		}
	}
}

func TestSanitizePath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"/v1/chat/completions", "/v1/chat/completions"},
		{"../../../etc/passwd", "/"},
		{"/../etc", "/etc"},
	}

	for _, tt := range tests {
		result := rewrite.SanitizePath(tt.input)
		if result != tt.expected {
			t.Errorf("SanitizePath(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}
