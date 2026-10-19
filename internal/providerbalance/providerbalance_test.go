package providerbalance

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

type memoryStore struct {
	account   Account
	snapshots []*Snapshot
}

func (s *memoryStore) ListBalanceAccounts(context.Context) ([]Account, error) {
	return []Account{s.account}, nil
}

func (s *memoryStore) GetBalanceAccount(context.Context, string) (*Account, error) {
	copy := s.account
	return &copy, nil
}

func (s *memoryStore) InsertBalanceSnapshot(_ context.Context, snapshot *Snapshot) error {
	copy := *snapshot
	copy.ID = int64(len(s.snapshots) + 1)
	snapshot.ID = copy.ID
	s.snapshots = append(s.snapshots, &copy)
	return nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestRefreshDeepSeekRecordsMonetaryBalance(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "deepseek-1", Provider: "deepseek-completions", UpstreamBaseURL: "https://api.deepseek.test",
		CredentialRef: "credential-1", BillingMode: "default", BillingCurrency: "USD", BalanceEnabled: true,
	}}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://api.deepseek.test/user/balance" {
			t.Fatalf("url = %q", req.URL.String())
		}
		if req.Header.Get("Authorization") != "Bearer secret" {
			t.Fatal("missing bearer credential")
		}
		return response(200, `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"20.00"},{"currency":"USD","total_balance":"12.34"}]}`), nil
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "secret"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "success" || snapshot.BalanceKind != "monetary" || snapshot.Currency != "USD" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.AmountRemaining == nil || *snapshot.AmountRemaining != 12.34 {
		t.Fatalf("amount = %v", snapshot.AmountRemaining)
	}
	if snapshot.BillingMode != "pay_as_you_go" {
		t.Fatalf("billing mode = %q", snapshot.BillingMode)
	}
	if !strings.Contains(string(snapshot.RawPayload), `"total_balance":"12.34"`) {
		t.Fatalf("raw payload was not preserved: %s", snapshot.RawPayload)
	}
}

func TestRefreshCodexRecordsRemainingUsageWindows(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "codex-1", Provider: "openai-codex-responses", UpstreamBaseURL: "https://chatgpt.com/backend-api/codex",
		CredentialRef: "credential-2", BillingMode: "subscription", BalanceEnabled: true,
	}}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://chatgpt.com/backend-api/wham/usage" {
			t.Fatalf("url = %q", req.URL.String())
		}
		if req.Header.Get("ChatGPT-Account-ID") != "acct-1" {
			t.Fatal("missing ChatGPT account header")
		}
		return response(200, `{
			"plan_type":"pro",
			"rate_limit":{"primary_window":{"used_percent":23,"limit_window_seconds":18000,"reset_at":1900000000},"secondary_window":{"used_percent":81,"limit_window_seconds":604800,"reset_at":1900600000}},
			"spend_control":{"individual_limit":{"remaining_percent":68,"reset_at":1900700000}}
		}`), nil
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "token", AccountID: "acct-1"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.BalanceKind != "usage" || snapshot.BillingMode != "subscription" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.AmountRemaining == nil || *snapshot.AmountRemaining != 19 {
		t.Fatalf("most restrictive remaining = %v", snapshot.AmountRemaining)
	}
	if snapshot.AmountLimit == nil || *snapshot.AmountLimit != 100 || snapshot.AmountUsed == nil || *snapshot.AmountUsed != 81 {
		t.Fatalf("normalized usage = limit %v used %v", snapshot.AmountLimit, snapshot.AmountUsed)
	}
	if len(snapshot.UsageWindows) != 3 || snapshot.UsageWindows[0].RemainingPercent != 77 || snapshot.UsageWindows[1].RemainingPercent != 19 {
		t.Fatalf("windows = %#v", snapshot.UsageWindows)
	}
	if !strings.Contains(string(snapshot.RawPayload), `"plan_type":"pro"`) {
		t.Fatalf("raw payload was not preserved: %s", snapshot.RawPayload)
	}
}

func TestRefreshZAIRecordsEveryUsageDocumentAndQuotaWindow(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "zai-coding", Provider: "openai-completions", CatalogKey: "zai",
		UpstreamBaseURL: "https://api.z.ai/api/coding/paas/v4",
		CredentialRef:   "credential-zai", BalanceEnabled: true,
	}}
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Header.Get("Authorization") != "Bearer zai-secret" {
			t.Fatalf("monitor authorization = %q", req.Header.Get("Authorization"))
		}
		switch req.URL.Path {
		case "/api/monitor/usage/quota/limit":
			return response(200, `{
				"code":200,"data":{"planName":"GLM Coding Pro","limits":[
					{"type":"CREDIT_LIMIT","unit":3,"number":5,"usage":100,"currentValue":25,"remaining":75,"percentage":25,"nextResetTime":1900000000000},
					{"type":"CREDIT_LIMIT","unit":6,"number":1,"usage":1000,"currentValue":700,"remaining":300,"percentage":70,"nextResetTime":1900500000000},
					{"type":"TIME_LIMIT","unit":5,"number":1,"usage":100,"currentValue":10,"remaining":90,"percentage":10,"nextResetTime":1900600000000}
				]}}
			`), nil
		case "/api/monitor/usage/model-usage":
			if req.URL.Query().Get("startTime") == "" || req.URL.Query().Get("endTime") == "" {
				t.Fatal("model usage request did not include a time range")
			}
			return response(200, `{"code":200,"success":true,"data":{"modelDataList":[{"modelName":"glm-5","tokensUsage":[123]}]}}`), nil
		case "/api/monitor/usage/tool-usage":
			if req.URL.Query().Get("startTime") == "" || req.URL.Query().Get("endTime") == "" {
				t.Fatal("tool usage request did not include a time range")
			}
			return response(200, `{"code":200,"success":true,"data":{"totalUsage":7}}`), nil
		default:
			t.Fatalf("unexpected ZAI usage URL %q", req.URL.String())
			return nil, nil
		}
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "zai-secret"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
	if snapshot.Status != "success" || snapshot.BillingMode != "subscription" || snapshot.BalanceKind != "usage" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.AmountRemaining == nil || *snapshot.AmountRemaining != 30 || snapshot.AmountUsed == nil || *snapshot.AmountUsed != 70 {
		t.Fatalf("normalized usage = used %v remaining %v", snapshot.AmountUsed, snapshot.AmountRemaining)
	}
	if len(snapshot.UsageWindows) != 3 || snapshot.UsageWindows[0].Name != "primary" ||
		snapshot.UsageWindows[1].Name != "secondary" || snapshot.UsageWindows[2].Name != "mcp" {
		t.Fatalf("windows = %#v", snapshot.UsageWindows)
	}
	if snapshot.UsageWindows[0].WindowSeconds != 5*60*60 || snapshot.UsageWindows[1].WindowSeconds != 7*24*60*60 {
		t.Fatalf("window durations = %#v", snapshot.UsageWindows)
	}
	raw := string(snapshot.RawPayload)
	for _, field := range []string{`"quota"`, `"model_usage"`, `"tool_usage"`, `"GLM Coding Pro"`, `"tokensUsage"`} {
		if !strings.Contains(raw, field) {
			t.Fatalf("raw payload missing %s: %s", field, raw)
		}
	}
	if snapshot.Source != "https://api.z.ai/api/monitor/usage/quota/limit" {
		t.Fatalf("source = %q", snapshot.Source)
	}
}

func TestRefreshZAIPreservesQuotaWhenDetailedUsageIsUnavailable(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "zai-general", Provider: "openai-completions", CatalogKey: "zai",
		UpstreamBaseURL: "https://api.z.ai/api/paas/v4",
		CredentialRef:   "credential-zai", BalanceEnabled: true,
	}}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/monitor/usage/quota/limit" {
			return response(200, `{"code":200,"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":40}]}}`), nil
		}
		return response(503, `{"message":"temporary reporting outage"}`), nil
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "zai-secret"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "success" || snapshot.AmountRemaining == nil || *snapshot.AmountRemaining != 60 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.BillingMode != "pay_as_you_go" {
		t.Fatalf("general API billing mode = %q", snapshot.BillingMode)
	}
	if strings.Contains(string(snapshot.RawPayload), "temporary reporting outage") {
		t.Fatalf("failed upstream response body was retained: %s", snapshot.RawPayload)
	}
}

func TestRefreshZAIFallsBackToOfficialRawTokenAuthorization(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "zai-coding", Provider: "openai-completions", CatalogKey: "zai",
		UpstreamBaseURL: "https://api.z.ai/api/coding/paas/v4",
		CredentialRef:   "credential-zai", BalanceEnabled: true,
	}}
	bearerRequests := 0
	rawRequests := 0
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") == "Bearer zai-secret" {
			bearerRequests++
			return response(200, `{"code":401,"success":false}`), nil
		}
		if req.Header.Get("Authorization") != "zai-secret" {
			t.Fatalf("monitor authorization = %q", req.Header.Get("Authorization"))
		}
		rawRequests++
		return response(200, `{"code":200,"success":true,"data":{"limits":[{"type":"TOKENS_LIMIT","unit":3,"number":5,"percentage":20}]}}`), nil
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "zai-secret"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bearerRequests != 1 || rawRequests != 3 {
		t.Fatalf("authorization attempts = bearer %d raw %d", bearerRequests, rawRequests)
	}
	if snapshot.AmountRemaining == nil || *snapshot.AmountRemaining != 80 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestRefreshZAIRecordsValidTokenWhenQuotaServiceIsUnavailable(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "zai-coding", Provider: "openai-completions", CatalogKey: "zai",
		UpstreamBaseURL: "https://api.z.ai/api/coding/paas/v4",
		CredentialRef:   "credential-zai", BalanceEnabled: true,
	}}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/api/monitor/usage/quota/limit":
			return response(200, `{"code":500,"success":false,"msg":"quota service unavailable"}`), nil
		case "/api/coding/paas/v4/models":
			if req.Header.Get("Authorization") != "Bearer zai-secret" {
				t.Fatalf("model authorization = %q", req.Header.Get("Authorization"))
			}
			return response(200, `{"object":"list","data":[{"id":"glm-5"}]}`), nil
		default:
			t.Fatalf("unexpected ZAI URL %q", req.URL.String())
			return nil, nil
		}
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "zai-secret"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "success" || snapshot.NormalizationError == "" || snapshot.BalanceKind != "" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if snapshot.Source != "https://api.z.ai/api/coding/paas/v4/models" {
		t.Fatalf("source = %q", snapshot.Source)
	}
	if !strings.Contains(string(snapshot.RawPayload), `"models"`) || !strings.Contains(string(snapshot.RawPayload), `"glm-5"`) {
		t.Fatalf("model response was not preserved: %s", snapshot.RawPayload)
	}
}

func TestRefreshZAIRejectsInvalidTokenWhenQuotaAndModelsFail(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "zai-coding", Provider: "openai-completions", CatalogKey: "zai",
		UpstreamBaseURL: "https://api.z.ai/api/coding/paas/v4",
		CredentialRef:   "credential-zai", BalanceEnabled: true,
	}}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/monitor/usage/quota/limit" {
			return response(200, `{"code":401,"success":false}`), nil
		}
		return response(401, `{"error":"invalid token"}`), nil
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "zai-secret"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err == nil {
		t.Fatal("expected invalid token to fail")
	}
	if snapshot.Status != "error" || snapshot.ErrorCode != "upstream_query_failed" || len(snapshot.RawPayload) != 0 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestRefreshRecordsSanitizedUpstreamError(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "deepseek-1", Provider: "deepseek", UpstreamBaseURL: "https://api.deepseek.test",
		CredentialRef: "credential-1", BalanceEnabled: true,
	}}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(401, `{"error":"secret details"}`), nil
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "secret"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if snapshot == nil || snapshot.Status != "error" || snapshot.ErrorCode != "upstream_query_failed" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if strings.Contains(snapshot.ErrorMessage, "secret details") {
		t.Fatalf("upstream response body leaked: %q", snapshot.ErrorMessage)
	}
}

func TestRefreshGenericSubscriptionUsesExplicitQuotaHeaders(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "qwen-plan", Provider: "openai-completions", UpstreamBaseURL: "https://token-plan.test/compatible-mode",
		CredentialRef: "credential-3", BillingMode: "subscription", BalanceEnabled: true,
		BalanceURL: "https://token-plan.test/plan-usage",
	}}
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://token-plan.test/plan-usage" {
			t.Fatalf("url = %q", req.URL.String())
		}
		resp := response(200, `{"data":[]}`)
		resp.Header.Set("X-Plan-Remaining-Credits", "7200")
		resp.Header.Set("X-Plan-Limit-Credits", "10000")
		resp.Header.Set("Set-Cookie", "secret=session")
		return resp, nil
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "secret"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Unit != "credits" || snapshot.AmountRemaining == nil || *snapshot.AmountRemaining != 7200 {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if len(snapshot.UsageWindows) != 1 || snapshot.UsageWindows[0].RemainingPercent != 72 {
		t.Fatalf("windows = %#v", snapshot.UsageWindows)
	}
	if snapshot.AmountLimit == nil || *snapshot.AmountLimit != 10000 || snapshot.AmountUsed == nil || *snapshot.AmountUsed != 2800 {
		t.Fatalf("normalized usage = limit %v used %v", snapshot.AmountLimit, snapshot.AmountUsed)
	}
	if snapshot.RawHeaders["X-Plan-Remaining-Credits"][0] != "7200" {
		t.Fatalf("quota headers were not preserved: %#v", snapshot.RawHeaders)
	}
	if _, exists := snapshot.RawHeaders["Set-Cookie"]; exists {
		t.Fatalf("sensitive header was preserved: %#v", snapshot.RawHeaders)
	}
	if string(snapshot.RawPayload) != `{"data":[]}` {
		t.Fatalf("raw payload was not preserved: %s", snapshot.RawPayload)
	}
}

func TestRefreshGenericSubscriptionWithoutBalanceAPIDoesNotQueryUpstream(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "qwen-plan", Provider: "openai-completions", UpstreamBaseURL: "https://token-plan.test/compatible-mode",
		CredentialRef: "credential-3", BillingMode: "subscription", BalanceEnabled: true,
	}}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("generic subscription must not be polled without an explicit balance endpoint")
		return nil, nil
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "secret"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err == nil {
		t.Fatal("expected unavailable balance API error")
	}
	if snapshot == nil || snapshot.ErrorCode != "balance_api_unavailable" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
}

func TestRefreshPreservesRawPayloadWhenNormalizationIsUnknown(t *testing.T) {
	store := &memoryStore{account: Account{
		ID: "codex-1", Provider: "openai-codex-responses", UpstreamBaseURL: "https://chatgpt.com/backend-api/codex",
		CredentialRef: "credential-2", BillingMode: "subscription", BalanceEnabled: true,
	}}
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(200, `{"new_usage_shape":{"tokens_left":12345},"plan_type":"pro"}`), nil
	})}
	worker := NewWorker(store, CredentialResolverFunc(func(context.Context, string) (*Credential, error) {
		return &Credential{Secret: "token"}, nil
	}), client, time.Minute, zerolog.Nop())

	snapshot, err := worker.RefreshAccount(context.Background(), store.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != "success" || snapshot.NormalizationError == "" {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	if !strings.Contains(string(snapshot.RawPayload), `"tokens_left":12345`) {
		t.Fatalf("unknown raw payload was not preserved: %s", snapshot.RawPayload)
	}
	if snapshot.NormalizationVersion != 1 {
		t.Fatalf("normalization version = %d", snapshot.NormalizationVersion)
	}
}
