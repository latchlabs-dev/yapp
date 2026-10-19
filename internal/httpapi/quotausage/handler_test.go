package quotausage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/usage"
)

type fakeLimitReader struct {
	limits map[string]int64
}

func (f *fakeLimitReader) GetEffectiveLimit(scopeType quota.ScopeType, scopeID string, limitType quota.LimitType) int64 {
	return f.limits[string(scopeType)+":"+scopeID+":"+string(limitType)]
}

type fakeCounterReader struct {
	connected bool
	values    map[string]int64
	err       error
}

func (f *fakeCounterReader) IsConnected() bool {
	return f.connected
}

func (f *fakeCounterReader) GetCounter(_ context.Context, key string) (int64, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.values[key], nil
}

type fakeCacheTrendReader struct {
	current  []usage.CacheTrendPoint
	previous []usage.CacheTrendPoint
	err      error
}

func (f *fakeCacheTrendReader) QueryCacheTrend(
	_ context.Context,
	_, _, _ string,
	from, _ time.Time,
) ([]usage.CacheTrendPoint, error) {
	if f.err != nil {
		return nil, f.err
	}
	if from.Day() == 18 {
		return f.current, nil
	}
	return f.previous, nil
}

func TestHandlerReturnsApplicableScopesAndTightestBalances(t *testing.T) {
	now := time.Date(2026, time.July, 24, 12, 30, 0, 0, time.UTC)
	limits := &fakeLimitReader{limits: map[string]int64{
		"api_key:key_1:daily_spend_cents":     500,
		"user:user_1:daily_spend_cents":       250,
		"tenant:tenant_1:daily_spend_cents":   1000,
		"api_key:key_1:monthly_spend_cents":   5000,
		"user:user_1:monthly_spend_cents":     4000,
		"tenant:tenant_1:monthly_spend_cents": 8000,
	}}
	counters := &fakeCounterReader{
		connected: true,
		values: map[string]int64{
			"quota:daily:spend_millicents:api_key:key_1:2026-07-24":   100_000,
			"quota:daily:spend_millicents:user:user_1:2026-07-24":     200_000,
			"quota:daily:spend_millicents:tenant:tenant_1:2026-07-24": 900_000,
			"quota:monthly:spend_millicents:api_key:key_1:2026-07":    1_000_000,
			"quota:monthly:spend_millicents:user:user_1:2026-07":      2_500_000,
			"quota:monthly:spend_millicents:tenant:tenant_1:2026-07":  7_000_000,
		},
	}
	cacheTrends := &fakeCacheTrendReader{
		current: []usage.CacheTrendPoint{
			{
				Day:              time.Date(2026, time.July, 23, 0, 0, 0, 0, time.UTC),
				RequestCount:     2,
				CachedRequests:   1,
				InputTokens:      100,
				CacheReadTokens:  300,
				CacheWriteTokens: 20,
			},
			{
				Day:             time.Date(2026, time.July, 24, 0, 0, 0, 0, time.UTC),
				RequestCount:    2,
				CachedRequests:  2,
				InputTokens:     100,
				CacheReadTokens: 100,
			},
		},
		previous: []usage.CacheTrendPoint{{
			RequestCount:    2,
			CachedRequests:  1,
			InputTokens:     300,
			CacheReadTokens: 100,
		}},
	}
	handler := newHandler(limits, counters, cacheTrends, func() time.Time { return now })
	req := authenticatedRequest()
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store, private" {
		t.Fatalf("Cache-Control = %q", got)
	}

	var response Response
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Currency != "USD" || !response.AsOf.Equal(now) {
		t.Fatalf("unexpected response metadata: %+v", response)
	}
	if len(response.Scopes) != 3 {
		t.Fatalf("scope count = %d, want 3", len(response.Scopes))
	}
	if response.Scopes[0].ScopeType != quota.ScopeKey ||
		response.Scopes[1].ScopeType != quota.ScopeUser ||
		response.Scopes[2].ScopeType != quota.ScopeTenant {
		t.Fatalf("scope order = %v", response.Scopes)
	}
	if response.Daily.BindingScopeType != quota.ScopeUser || response.Daily.BindingScopeID != "user_1" {
		t.Fatalf("daily binding scope = %s/%s", response.Daily.BindingScopeType, response.Daily.BindingScopeID)
	}
	if response.Daily.UsedCents != 200 || response.Daily.RemainingCents == nil || *response.Daily.RemainingCents != 50 {
		t.Fatalf("unexpected effective daily balance: %+v", response.Daily)
	}
	if response.Daily.UsedPercent == nil || *response.Daily.UsedPercent != 80 ||
		response.Daily.RemainingPercent == nil || *response.Daily.RemainingPercent != 20 {
		t.Fatalf("unexpected effective daily percentages: %+v", response.Daily)
	}
	if response.Monthly.BindingScopeType != quota.ScopeTenant || response.Monthly.BindingScopeID != "tenant_1" {
		t.Fatalf("monthly binding scope = %s/%s", response.Monthly.BindingScopeType, response.Monthly.BindingScopeID)
	}
	if response.Monthly.UsedCents != 7000 || response.Monthly.RemainingCents == nil || *response.Monthly.RemainingCents != 1000 {
		t.Fatalf("unexpected effective monthly balance: %+v", response.Monthly)
	}
	if response.Monthly.UsedPercent == nil || *response.Monthly.UsedPercent != 87.5 ||
		response.Monthly.RemainingPercent == nil || *response.Monthly.RemainingPercent != 12.5 {
		t.Fatalf("unexpected effective monthly percentages: %+v", response.Monthly)
	}
	if want := time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC); !response.Daily.ResetsAt.Equal(want) {
		t.Fatalf("daily reset = %s, want %s", response.Daily.ResetsAt, want)
	}
	if want := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC); !response.Monthly.ResetsAt.Equal(want) {
		t.Fatalf("monthly reset = %s, want %s", response.Monthly.ResetsAt, want)
	}
	if !response.CacheTrend.Available || response.CacheTrend.ScopeType != quota.ScopeUser {
		t.Fatalf("unexpected cache trend metadata: %+v", response.CacheTrend)
	}
	if response.CacheTrend.Current.CacheReadSharePercent != 66.7 ||
		response.CacheTrend.Current.RequestCacheHitPercent != 75 {
		t.Fatalf("unexpected current cache trend: %+v", response.CacheTrend.Current)
	}
	if response.CacheTrend.Previous.CacheReadSharePercent != 25 ||
		response.CacheTrend.CacheReadShareChangePoints != 41.7 ||
		response.CacheTrend.Direction != "up" {
		t.Fatalf("unexpected cache comparison: %+v", response.CacheTrend)
	}
	if len(response.CacheTrend.Daily) != 7 ||
		response.CacheTrend.Daily[6].Date != "2026-07-24" ||
		response.CacheTrend.Daily[6].CacheReadTokens != 100 {
		t.Fatalf("unexpected daily cache trend: %+v", response.CacheTrend.Daily)
	}
}

func TestHandlerRequiresAuthentication(t *testing.T) {
	handler := newHandler(
		&fakeLimitReader{},
		&fakeCounterReader{connected: true},
		nil,
		time.Now,
	)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/quota", nil)

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestHandlerFailsClosedWhenCountersUnavailable(t *testing.T) {
	tests := []struct {
		name     string
		counters *fakeCounterReader
	}{
		{name: "disconnected", counters: &fakeCounterReader{}},
		{name: "read error", counters: &fakeCounterReader{connected: true, err: errors.New("boom")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newHandler(&fakeLimitReader{}, tt.counters, nil, time.Now)
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, authenticatedRequest())

			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var response map[string]map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response["error"]["type"] != "quota_unavailable" {
				t.Fatalf("error response = %#v", response)
			}
		})
	}
}

func TestPeriodUsagePreservesSubCentPrecisionAndExhaustion(t *testing.T) {
	reset := time.Date(2026, time.July, 25, 0, 0, 0, 0, time.UTC)
	usage := periodUsage(1_234, 1, reset)

	if usage.UsedCents != 1.234 {
		t.Fatalf("used cents = %v", usage.UsedCents)
	}
	if usage.RemainingMillicents == nil || *usage.RemainingMillicents != 0 {
		t.Fatalf("remaining millicents = %v", usage.RemainingMillicents)
	}
	if usage.UsedPercent == nil || *usage.UsedPercent != 123.4 {
		t.Fatalf("used percent = %v", usage.UsedPercent)
	}
	if usage.RemainingPercent == nil || *usage.RemainingPercent != 0 {
		t.Fatalf("remaining percent = %v", usage.RemainingPercent)
	}
	if !usage.Exhausted {
		t.Fatal("expected exhausted balance")
	}
}

func TestPeriodUsageLeavesPercentagesEmptyForUnlimitedQuota(t *testing.T) {
	usage := periodUsage(12_345, 0, time.Now())

	if !usage.Unlimited {
		t.Fatal("expected unlimited balance")
	}
	if usage.UsedPercent != nil || usage.RemainingPercent != nil {
		t.Fatalf("unlimited percentages = used %v, remaining %v", usage.UsedPercent, usage.RemainingPercent)
	}
}

func authenticatedRequest() *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/proxy/v1/quota", nil)
	return req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tenant_1",
		UserID:   "user_1",
		KeyID:    "key_1",
	}))
}
