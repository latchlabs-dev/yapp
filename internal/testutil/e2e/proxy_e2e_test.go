// Package e2e_test drives the proxy data path end to end against in-memory mock
// upstreams. It exercises a real ChatHandler — request rewriting, upstream
// credential injection, forwarding, and usage recording — over a real HTTP
// round trip, and confirms the tokens the proxy sends upstream are exactly what
// we expect:
//
//   - the upstream *auth* token: the proxy injects the configured provider
//     credential as "Bearer <secret>" and never leaks the client's own token;
//   - the upstream *usage* tokens: the proxy parses the upstream's reported
//     token counts (including DeepSeek's cache-hit/miss split) and records them.
package e2e_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/secrets"
	"github.com/earlgrey/piproxy/internal/testutil/mockupstream"
	"github.com/earlgrey/piproxy/internal/upstream"
	"github.com/earlgrey/piproxy/internal/usage"
)

const (
	openaiUpstreamToken   = "sk-openai-upstream-SECRET"
	deepseekUpstreamToken = "sk-deepseek-upstream-SECRET"
	// clientToken is what a caller sends to the proxy. It must never reach an
	// upstream — the proxy strips it and injects its own credential.
	clientToken = "Bearer caller-token-DO-NOT-FORWARD"
)

// proxyFixture bundles the running proxy and its mock upstreams.
type proxyFixture struct {
	proxy        *httptest.Server
	openaiMock   *mockupstream.Server
	deepseekMock *mockupstream.Server
	events       <-chan *usage.Event
}

func newProxyFixture(t *testing.T) *proxyFixture {
	t.Helper()

	openaiMock := mockupstream.NewOpenAI()
	deepseekMock := mockupstream.NewDeepSeek()
	t.Cleanup(openaiMock.Close)
	t.Cleanup(deepseekMock.Close)

	// In-memory credential storage, keyed by the providers' CredentialID.
	store := &memSecretStore{data: map[string][]byte{
		"openai-upstream":   []byte(openaiUpstreamToken),
		"deepseek-upstream": []byte(deepseekUpstreamToken),
	}}
	creds := secrets.NewCredentialStore(store)

	registry := providers.NewRegistry()
	if err := registry.RegisterProvider(&providers.Provider{
		Type:               providers.ProviderOpenAI,
		Name:               "Mock OpenAI",
		PublicBasePath:     "/openai",
		UpstreamBaseURL:    openaiMock.URL,
		AllowedPaths:       []string{"/v1/chat/completions"},
		CredentialID:       "openai-upstream",
		AllowPrivateEgress: true,
		Streaming:          true,
		Enabled:            true,
		Timeout:            5 * time.Second,
	}); err != nil {
		t.Fatalf("register openai provider: %v", err)
	}

	// ApplyCatalogDefaults supplies the DeepSeek model map so corp-* model IDs
	// route to their upstream IDs, while we keep the mock URL and credential.
	deepseek := providers.ApplyCatalogDefaults(&providers.Provider{
		Type:               providers.ProviderDeepSeek,
		PublicBasePath:     "/deepseek",
		UpstreamBaseURL:    deepseekMock.URL,
		AllowedPaths:       []string{"/v1/chat/completions"},
		CredentialID:       "deepseek-upstream",
		AllowPrivateEgress: true,
		Enabled:            true,
		Timeout:            5 * time.Second,
	}, providers.CatalogDeepSeek)
	if err := registry.RegisterProvider(deepseek); err != nil {
		t.Fatalf("register deepseek provider: %v", err)
	}

	recorder := usage.NewRecorder(usage.NewCostTable(), nil, nil, 0, zerolog.Nop())

	svcs := dataplane.Services{
		Credentials: creds,
		Usage:       recorder,
		CostTable:   usage.NewCostTable(),
	}
	chatHandler := openai.NewChatHandler(
		upstream.NewClient(upstream.Config{Timeout: 5 * time.Second, MaxConnsPerHost: 10, MaxIdleConns: 10}),
		rewrite.NewRequestRewriter(registry),
		registry,
		svcs,
	)

	router := chi.NewRouter()
	router.Use(middleware.RequestID)
	router.Post("/openai/v1/chat/completions", chatHandler.Handle)
	router.Post("/deepseek/v1/chat/completions", chatHandler.Handle)

	proxy := httptest.NewServer(router)
	t.Cleanup(proxy.Close)

	return &proxyFixture{
		proxy:        proxy,
		openaiMock:   openaiMock,
		deepseekMock: deepseekMock,
		events:       recorder.Events(),
	}
}

func (f *proxyFixture) post(t *testing.T, path, body string, stream bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.proxy.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// The caller authenticates to the proxy with its own token; it must not be
	// forwarded upstream.
	req.Header.Set("Authorization", clientToken)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func (f *proxyFixture) nextUsageEvent(t *testing.T) *usage.Event {
	t.Helper()
	select {
	case e := <-f.events:
		return e
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for usage event")
		return nil
	}
}

// TestOpenAIUpstreamTokens confirms an OpenAI request reaches the upstream with
// the injected upstream credential (not the client's token) and that the
// upstream's reported usage tokens are recorded.
func TestOpenAIUpstreamTokens(t *testing.T) {
	f := newProxyFixture(t)

	resp := f.post(t, "/openai/v1/chat/completions",
		`{"model":"gpt-4o-mini","messages":[{"role":"user","content":"hi"}]}`, false)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), "mock OpenAI upstream") {
		t.Fatalf("response body did not come from mock upstream: %s", respBody)
	}

	got, ok := f.openaiMock.LastRequest()
	if !ok {
		t.Fatal("openai upstream received no request")
	}

	// Auth token: proxy injected its configured upstream credential as Bearer.
	if want := "Bearer " + openaiUpstreamToken; got.Authorization != want {
		t.Errorf("upstream Authorization = %q, want %q", got.Authorization, want)
	}
	if got.BearerToken != openaiUpstreamToken {
		t.Errorf("upstream bearer token = %q, want %q", got.BearerToken, openaiUpstreamToken)
	}
	// The client's own token must never be forwarded.
	if strings.Contains(got.Authorization, "caller-token") {
		t.Errorf("client token leaked upstream: %q", got.Authorization)
	}
	if got.Model != "gpt-4o-mini" {
		t.Errorf("upstream model = %q, want gpt-4o-mini", got.Model)
	}

	// Usage tokens: the proxy recorded what the upstream reported.
	ev := f.nextUsageEvent(t)
	if ev.InputTokens != 11 || ev.OutputTokens != 7 || ev.TotalTokens != 18 {
		t.Errorf("usage = in:%d out:%d total:%d, want in:11 out:7 total:18",
			ev.InputTokens, ev.OutputTokens, ev.TotalTokens)
	}
	if ev.EstimatedUsage {
		t.Error("usage was estimated; expected exact counts from upstream")
	}
	if ev.Model != "gpt-4o-mini" || ev.Provider != string(providers.ProviderOpenAI) {
		t.Errorf("usage event model/provider = %q/%q, want gpt-4o-mini/openai", ev.Model, ev.Provider)
	}
}

// TestDeepSeekUpstreamTokens confirms a DeepSeek request: the corp-* model is
// rewritten to its upstream ID, the DeepSeek credential is injected, and the
// cache-hit/miss usage split is parsed into input + cache-read tokens.
func TestDeepSeekUpstreamTokens(t *testing.T) {
	f := newProxyFixture(t)

	resp := f.post(t, "/deepseek/v1/chat/completions",
		`{"model":"corp-deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}]}`, false)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), "reasoning_content") {
		t.Fatalf("expected DeepSeek-style response with reasoning_content: %s", respBody)
	}

	got, ok := f.deepseekMock.LastRequest()
	if !ok {
		t.Fatal("deepseek upstream received no request")
	}

	if want := "Bearer " + deepseekUpstreamToken; got.Authorization != want {
		t.Errorf("upstream Authorization = %q, want %q", got.Authorization, want)
	}
	if strings.Contains(got.Authorization, "caller-token") {
		t.Errorf("client token leaked upstream: %q", got.Authorization)
	}
	// The proxy rewrites the public corp-* model to the upstream ID.
	if got.Model != "deepseek-flash" {
		t.Errorf("upstream model = %q, want deepseek-flash", got.Model)
	}

	ev := f.nextUsageEvent(t)
	// DeepSeek reports 12 cache-miss + 8 cache-hit prompt tokens; the proxy maps
	// misses to input tokens and hits to cache-read tokens.
	if ev.InputTokens != 12 || ev.CacheReadTokens != 8 || ev.OutputTokens != 9 || ev.TotalTokens != 29 {
		t.Errorf("usage = in:%d cacheRead:%d out:%d total:%d, want in:12 cacheRead:8 out:9 total:29",
			ev.InputTokens, ev.CacheReadTokens, ev.OutputTokens, ev.TotalTokens)
	}
	if ev.Model != "deepseek-flash" || ev.Provider != string(providers.ProviderDeepSeek) {
		t.Errorf("usage event model/provider = %q/%q, want deepseek-flash/deepseek", ev.Model, ev.Provider)
	}
}

// TestStreamingUpstreamTokens confirms the streaming path injects the upstream
// token and extracts usage from the SSE stream.
func TestStreamingUpstreamTokens(t *testing.T) {
	f := newProxyFixture(t)

	resp := f.post(t, "/openai/v1/chat/completions",
		`{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}`, true)
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, respBody)
	}
	if !strings.Contains(string(respBody), "data:") {
		t.Fatalf("expected SSE stream, got: %s", respBody)
	}

	got, ok := f.openaiMock.LastRequest()
	if !ok {
		t.Fatal("openai upstream received no request")
	}
	if !got.Stream {
		t.Error("upstream did not receive a streaming request")
	}
	if want := "Bearer " + openaiUpstreamToken; got.Authorization != want {
		t.Errorf("upstream Authorization = %q, want %q", got.Authorization, want)
	}

	ev := f.nextUsageEvent(t)
	if ev.InputTokens != 11 || ev.OutputTokens != 7 || ev.TotalTokens != 18 {
		t.Errorf("stream usage = in:%d out:%d total:%d, want in:11 out:7 total:18",
			ev.InputTokens, ev.OutputTokens, ev.TotalTokens)
	}
	if ev.EstimatedUsage {
		t.Error("stream usage was estimated; expected exact counts from upstream SSE")
	}
}

// memSecretStore is a minimal in-memory secrets.SecretStore for tests.
type memSecretStore struct {
	data map[string][]byte
}

func (m *memSecretStore) Get(_ context.Context, ref string) ([]byte, error) {
	v, ok := m.data[ref]
	if !ok {
		return nil, secrets.ErrNotFound
	}
	return v, nil
}

func (m *memSecretStore) Put(_ context.Context, ref string, data []byte) error {
	m.data[ref] = data
	return nil
}

func (m *memSecretStore) Delete(_ context.Context, ref string) error {
	delete(m.data, ref)
	return nil
}

func (m *memSecretStore) Health(_ context.Context) error { return nil }
