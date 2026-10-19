package usage

import (
	"context"
	"time"
)

// CacheTrendPoint is one UTC day of cache-related input usage.
type CacheTrendPoint struct {
	Day              time.Time `json:"day"`
	RequestCount     int64     `json:"request_count"`
	CachedRequests   int64     `json:"cached_requests"`
	InputTokens      int64     `json:"input_tokens"`
	CacheReadTokens  int64     `json:"cache_read_tokens"`
	CacheWriteTokens int64     `json:"cache_write_tokens"`
}

// CacheTrendReader loads cache usage for one authenticated tenant/user/key
// identity over an exact time window.
type CacheTrendReader interface {
	QueryCacheTrend(
		ctx context.Context,
		tenantID, userID, keyID string,
		from, to time.Time,
	) ([]CacheTrendPoint, error)
}
