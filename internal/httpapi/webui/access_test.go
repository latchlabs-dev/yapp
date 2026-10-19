package webui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/ratelimit"
)

func anonymousRequest() *http.Request {
	return httptest.NewRequest(http.MethodGet, "/web/v1/x", nil)
}

func requestWithScopes(scopes ...string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/web/v1/x", nil)
	authCtx := &auth.Context{TenantID: "tnt_1", UserID: "usr_1", Scopes: scopes}
	return req.WithContext(middleware.WithAuthContext(req.Context(), authCtx))
}

// The console mounts outside the API-key auth group, so this middleware is the
// only thing standing between a session's snapshotted scopes and the route.
func TestRequireScopeAllowsMatchingSession(t *testing.T) {
	handler := requireScope(auth.ScopeMemoryObserve)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, requestWithScopes(auth.ScopeLLMInvoke, auth.ScopeMemoryObserve))

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}
}

func TestRequireScopeRejectsMissingScope(t *testing.T) {
	handler := requireScope(auth.ScopeMemoryObserve)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the guarded handler ran for a session without the scope")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, requestWithScopes(auth.ScopeLLMModelsRead))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestRequireScopeRejectsAnonymousRequest(t *testing.T) {
	handler := requireScope(auth.ScopeMemoryObserve)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the guarded handler ran without an auth context")
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, anonymousRequest())

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// A session scoped to one provider family must not be able to run turns on the
// other family's protocol, matching the CLI's per-family middleware.
func TestHasTurnScopeHonoursFamilies(t *testing.T) {
	cases := []struct {
		name   string
		scope  string
		family providers.ProviderType
		want   bool
	}{
		{"general scope allows openai", auth.ScopeLLMInvoke, providers.ProviderOpenAI, true},
		{"general scope allows anthropic", auth.ScopeLLMInvoke, providers.ProviderAnthropic, true},
		{"general scope allows deepseek", auth.ScopeLLMInvoke, providers.ProviderDeepSeek, true},
		{"anthropic scope allows anthropic", auth.ScopeLLMInvokeAnthropic, providers.ProviderAnthropic, true},
		{"anthropic scope denies openai", auth.ScopeLLMInvokeAnthropic, providers.ProviderOpenAI, false},
		{"openai scope denies anthropic", auth.ScopeLLMInvokeOpenAI, providers.ProviderAnthropic, false},
		{"deepseek scope allows deepseek", auth.ScopeLLMInvokeDeepSeek, providers.ProviderDeepSeek, true},
		{"deepseek scope denies openai", auth.ScopeLLMInvokeDeepSeek, providers.ProviderOpenAI, false},
		{"openai scope denies deepseek", auth.ScopeLLMInvokeOpenAI, providers.ProviderDeepSeek, false},
		{"read-only baseline is not invoke", auth.ScopeLLMModelsRead, providers.ProviderOpenAI, false},
		{"unknown provider denies even invoke", auth.ScopeLLMInvoke, providers.ProviderOpenAIResponses, false},
	}
	for _, tc := range cases {
		authCtx := &auth.Context{Scopes: []string{tc.scope}}
		if got := hasTurnScope(authCtx, tc.family); got != tc.want {
			t.Errorf("%s: hasTurnScope = %v, want %v", tc.name, got, tc.want)
		}
	}

	if hasTurnScope(nil, providers.ProviderOpenAI) {
		t.Error("a nil auth context must not be allowed to run turns")
	}
}

// The write guard exists because a user message is persisted before the
// delegated handler checks quota, so a denied turn still leaves a row.
func TestCheckWriteBudgetEnforcesPerUserLimit(t *testing.T) {
	deps := &Dependencies{
		Config:       Config{WriteRequestsPerMinute: 2},
		WriteLimiter: ratelimit.NewMemoryStore(),
	}
	ctx := context.Background()
	alice := &auth.Context{TenantID: "tnt_1", UserID: "usr_alice"}
	bob := &auth.Context{TenantID: "tnt_1", UserID: "usr_bob"}

	if !deps.checkWriteBudget(ctx, alice) {
		t.Fatal("the first write must be allowed")
	}
	if !deps.checkWriteBudget(ctx, alice) {
		t.Fatal("the second write must be allowed")
	}
	if deps.checkWriteBudget(ctx, alice) {
		t.Error("the third write within the window must be refused")
	}
	// The budget is per user, not per tenant.
	if !deps.checkWriteBudget(ctx, bob) {
		t.Error("one user's exhausted budget must not affect another's")
	}
}

func TestCheckWriteBudgetDegradesOpen(t *testing.T) {
	disabled := &Dependencies{
		Config:       Config{WriteRequestsPerMinute: 0},
		WriteLimiter: ratelimit.NewMemoryStore(),
	}
	authCtx := &auth.Context{TenantID: "tnt_1", UserID: "usr_1"}
	for i := 0; i < 100; i++ {
		if !disabled.checkWriteBudget(context.Background(), authCtx) {
			t.Fatal("a limit of 0 must disable the guard")
		}
	}

	if !(&Dependencies{}).checkWriteBudget(context.Background(), authCtx) {
		t.Error("a missing limiter must degrade open")
	}
	if !disabled.checkWriteBudget(context.Background(), nil) {
		t.Error("a missing auth context must degrade open rather than panic")
	}
}
