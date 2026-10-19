// Package quotausage exposes the authenticated caller's live spend balances.
package quotausage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	apierrors "github.com/earlgrey/piproxy/internal/errors"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/usage"
)

// LimitReader resolves the exact limits used by the data-plane enforcer.
type LimitReader interface {
	GetEffectiveLimit(scopeType quota.ScopeType, scopeID string, limitType quota.LimitType) int64
}

// CounterReader reads the distributed counters used by the quota enforcer.
type CounterReader interface {
	IsConnected() bool
	GetCounter(ctx context.Context, key string) (int64, error)
}

// PeriodUsage is a live spend balance for one UTC quota period. Millicent
// fields are exact; cent fields are convenient decimal representations, and
// percentage fields let clients present shared user/tenant limits without
// displaying a global dollar amount.
type PeriodUsage struct {
	UsedMillicents      int64     `json:"used_millicents"`
	UsedCents           float64   `json:"used_cents"`
	UsedPercent         *float64  `json:"used_percent"`
	LimitCents          int64     `json:"limit_cents"`
	RemainingMillicents *int64    `json:"remaining_millicents"`
	RemainingCents      *float64  `json:"remaining_cents"`
	RemainingPercent    *float64  `json:"remaining_percent"`
	ResetsAt            time.Time `json:"resets_at"`
	Unlimited           bool      `json:"unlimited"`
	Exhausted           bool      `json:"exhausted"`
}

// ScopeUsage contains balances for one of the scopes that constrains the
// authenticated key. The order is API key, user, then tenant, matching request
// enforcement.
type ScopeUsage struct {
	ScopeType quota.ScopeType `json:"scope_type"`
	ScopeID   string          `json:"scope_id"`
	Daily     PeriodUsage     `json:"daily"`
	Monthly   PeriodUsage     `json:"monthly"`
}

// EffectiveUsage is the tightest remaining balance across all applicable
// scopes. It is the amount a subsequent request can spend before any one scope
// reaches its limit.
type EffectiveUsage struct {
	PeriodUsage
	BindingScopeType quota.ScopeType `json:"binding_scope_type,omitempty"`
	BindingScopeID   string          `json:"binding_scope_id,omitempty"`
}

// Response is returned by GET /proxy/v1/quota.
type Response struct {
	Currency   string             `json:"currency"`
	AsOf       time.Time          `json:"as_of"`
	Daily      EffectiveUsage     `json:"daily"`
	Monthly    EffectiveUsage     `json:"monthly"`
	Scopes     []ScopeUsage       `json:"scopes"`
	CacheTrend CacheTrendResponse `json:"cache_trend"`
}

// CacheTrendStats summarizes cache behavior over one comparison window.
type CacheTrendStats struct {
	RequestCount           int64   `json:"request_count"`
	CachedRequests         int64   `json:"cached_requests"`
	InputTokens            int64   `json:"input_tokens"`
	CacheReadTokens        int64   `json:"cache_read_tokens"`
	CacheWriteTokens       int64   `json:"cache_write_tokens"`
	CacheReadSharePercent  float64 `json:"cache_read_share_percent"`
	RequestCacheHitPercent float64 `json:"request_cache_hit_percent"`
}

// CacheTrendDay is one of the seven UTC calendar-day points returned to clients.
type CacheTrendDay struct {
	Date string `json:"date"`
	CacheTrendStats
}

// CacheTrendResponse compares the current seven-day-to-date window with the
// same elapsed window one week earlier.
type CacheTrendResponse struct {
	Available                  bool            `json:"available"`
	ScopeType                  quota.ScopeType `json:"scope_type,omitempty"`
	Days                       int             `json:"days"`
	Current                    CacheTrendStats `json:"current"`
	Previous                   CacheTrendStats `json:"previous"`
	CacheReadShareChangePoints float64         `json:"cache_read_share_change_points"`
	Direction                  string          `json:"direction"`
	Daily                      []CacheTrendDay `json:"daily"`
}

// NewHandler returns the authenticated caller quota handler.
func NewHandler(limits LimitReader, counters CounterReader, cacheTrends usage.CacheTrendReader) http.HandlerFunc {
	return newHandler(limits, counters, cacheTrends, time.Now)
}

func newHandler(
	limits LimitReader,
	counters CounterReader,
	cacheTrends usage.CacheTrendReader,
	nowFn func() time.Time,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store, private")

		scopes := dataplane.ResolveQuotaScopes(r)
		if len(scopes) == 0 {
			apierrors.WriteErrorJSON(w, apierrors.ErrorUnauthorized, "authentication required")
			return
		}
		if limits == nil || counters == nil || !counters.IsConnected() {
			writeUnavailable(w, "quota counters are unavailable")
			return
		}

		now := nowFn().UTC()
		day := now.Format("2006-01-02")
		month := now.Format("2006-01")
		dailyReset := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		monthlyReset := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)

		scopeUsage := make([]ScopeUsage, 0, len(scopes))
		for _, scope := range scopes {
			dailyUsed, err := counters.GetCounter(
				r.Context(),
				fmt.Sprintf("quota:daily:spend_millicents:%s:%s:%s", scope.Type, scope.ID, day),
			)
			if err != nil {
				writeUnavailable(w, "failed to read daily quota counter")
				return
			}
			monthlyUsed, err := counters.GetCounter(
				r.Context(),
				fmt.Sprintf("quota:monthly:spend_millicents:%s:%s:%s", scope.Type, scope.ID, month),
			)
			if err != nil {
				writeUnavailable(w, "failed to read monthly quota counter")
				return
			}

			scopeUsage = append(scopeUsage, ScopeUsage{
				ScopeType: scope.Type,
				ScopeID:   scope.ID,
				Daily: periodUsage(
					dailyUsed,
					limits.GetEffectiveLimit(scope.Type, scope.ID, quota.LimitDailySpendCents),
					dailyReset,
				),
				Monthly: periodUsage(
					monthlyUsed,
					limits.GetEffectiveLimit(scope.Type, scope.ID, quota.LimitMonthlySpendCents),
					monthlyReset,
				),
			})
		}

		response := Response{
			Currency:   "USD",
			AsOf:       now,
			Daily:      effectiveUsage(scopeUsage, true, dailyReset),
			Monthly:    effectiveUsage(scopeUsage, false, monthlyReset),
			Scopes:     scopeUsage,
			CacheTrend: buildCacheTrend(r.Context(), cacheTrends, scopes, now),
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(response)
	}
}

func buildCacheTrend(
	ctx context.Context,
	reader usage.CacheTrendReader,
	scopes []dataplane.QuotaScope,
	now time.Time,
) CacheTrendResponse {
	result := CacheTrendResponse{
		Days:      7,
		Direction: "flat",
		Daily:     make([]CacheTrendDay, 0, 7),
	}
	if reader == nil {
		return result
	}

	var tenantID, userID, keyID string
	for _, scope := range scopes {
		switch scope.Type {
		case quota.ScopeTenant:
			tenantID = scope.ID
		case quota.ScopeUser:
			userID = scope.ID
		case quota.ScopeKey:
			keyID = scope.ID
		}
	}
	if userID != "" {
		result.ScopeType = quota.ScopeUser
	} else {
		result.ScopeType = quota.ScopeKey
	}

	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	currentStart := today.AddDate(0, 0, -6)
	previousStart := currentStart.AddDate(0, 0, -7)
	previousEnd := now.AddDate(0, 0, -7)

	currentPoints, err := reader.QueryCacheTrend(ctx, tenantID, userID, keyID, currentStart, now)
	if err != nil {
		return result
	}
	previousPoints, err := reader.QueryCacheTrend(ctx, tenantID, userID, keyID, previousStart, previousEnd)
	if err != nil {
		return result
	}

	byDay := make(map[string]usage.CacheTrendPoint, len(currentPoints))
	for _, point := range currentPoints {
		byDay[point.Day.UTC().Format("2006-01-02")] = point
	}
	for i := 0; i < 7; i++ {
		date := currentStart.AddDate(0, 0, i).Format("2006-01-02")
		point := byDay[date]
		result.Daily = append(result.Daily, CacheTrendDay{
			Date:            date,
			CacheTrendStats: summarizeCachePoints([]usage.CacheTrendPoint{point}),
		})
	}

	result.Current = summarizeCachePoints(currentPoints)
	result.Previous = summarizeCachePoints(previousPoints)
	result.CacheReadShareChangePoints = roundPercent(
		result.Current.CacheReadSharePercent - result.Previous.CacheReadSharePercent,
	)
	switch {
	case result.CacheReadShareChangePoints > 0:
		result.Direction = "up"
	case result.CacheReadShareChangePoints < 0:
		result.Direction = "down"
	}
	result.Available = true
	return result
}

func summarizeCachePoints(points []usage.CacheTrendPoint) CacheTrendStats {
	var result CacheTrendStats
	for _, point := range points {
		result.RequestCount += point.RequestCount
		result.CachedRequests += point.CachedRequests
		result.InputTokens += point.InputTokens
		result.CacheReadTokens += point.CacheReadTokens
		result.CacheWriteTokens += point.CacheWriteTokens
	}
	cacheableInput := result.InputTokens + result.CacheReadTokens
	if cacheableInput > 0 {
		result.CacheReadSharePercent = roundPercent(
			float64(result.CacheReadTokens) / float64(cacheableInput) * 100,
		)
	}
	if result.RequestCount > 0 {
		result.RequestCacheHitPercent = roundPercent(
			float64(result.CachedRequests) / float64(result.RequestCount) * 100,
		)
	}
	return result
}

func roundPercent(value float64) float64 {
	return math.Round(value*10) / 10
}

func periodUsage(usedMillicents, limitCents int64, resetsAt time.Time) PeriodUsage {
	if usedMillicents < 0 {
		usedMillicents = 0
	}
	result := PeriodUsage{
		UsedMillicents: usedMillicents,
		UsedCents:      float64(usedMillicents) / float64(quota.SpendCounterScale),
		LimitCents:     limitCents,
		ResetsAt:       resetsAt,
		Unlimited:      limitCents <= 0,
	}
	if limitCents <= 0 {
		return result
	}

	remainingMillicents := limitCents*quota.SpendCounterScale - usedMillicents
	if remainingMillicents < 0 {
		remainingMillicents = 0
	}
	remainingCents := float64(remainingMillicents) / float64(quota.SpendCounterScale)
	limitMillicents := limitCents * quota.SpendCounterScale
	usedPercent := roundPercent(float64(usedMillicents) / float64(limitMillicents) * 100)
	remainingPercent := roundPercent(float64(remainingMillicents) / float64(limitMillicents) * 100)
	result.RemainingMillicents = &remainingMillicents
	result.RemainingCents = &remainingCents
	result.UsedPercent = &usedPercent
	result.RemainingPercent = &remainingPercent
	result.Exhausted = usedMillicents >= limitMillicents
	return result
}

func effectiveUsage(scopes []ScopeUsage, daily bool, resetsAt time.Time) EffectiveUsage {
	result := EffectiveUsage{
		PeriodUsage: PeriodUsage{
			ResetsAt:  resetsAt,
			Unlimited: true,
		},
	}
	var tightestRemaining int64
	found := false
	for _, scope := range scopes {
		period := scope.Monthly
		if daily {
			period = scope.Daily
		}
		if period.Unlimited || period.RemainingMillicents == nil {
			continue
		}
		if found && *period.RemainingMillicents >= tightestRemaining {
			continue
		}
		found = true
		tightestRemaining = *period.RemainingMillicents
		result = EffectiveUsage{
			PeriodUsage:      period,
			BindingScopeType: scope.ScopeType,
			BindingScopeID:   scope.ScopeID,
		}
	}
	return result
}

func writeUnavailable(w http.ResponseWriter, message string) {
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"error": {
			"type":    "quota_unavailable",
			"message": message,
		},
	})
}
