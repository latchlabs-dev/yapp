package redis

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Lua script for sliding window counter: adds a weighted event, removes expired
// timestamps, returns the current weighted total atomically.
//
// KEYS[1] = sorted set key
// KEYS[2] = hash of event member -> weight
// KEYS[3] = sequence key used to make event members unique
// ARGV[1] = current unix timestamp (milliseconds)
// ARGV[2] = window size in milliseconds
// ARGV[3] = event weight
const slidingWindowScript = `
		local now = tonumber(ARGV[1])
		local window = tonumber(ARGV[2])
		local amount = tonumber(ARGV[3])
		local cutoff = now - window

		local expired = redis.call("ZRANGEBYSCORE", KEYS[1], "-inf", cutoff)
		if #expired > 0 then
			redis.call("HDEL", KEYS[2], unpack(expired))
		end
		redis.call("ZREMRANGEBYSCORE", KEYS[1], "-inf", cutoff)

		local seq = redis.call("INCR", KEYS[3])
		local member = now .. ":" .. seq
		redis.call("ZADD", KEYS[1], now, member)
		redis.call("HSET", KEYS[2], member, amount)

		local members = redis.call("ZRANGE", KEYS[1], 0, -1)
		local total = 0
		for _, active_member in ipairs(members) do
			total = total + (tonumber(redis.call("HGET", KEYS[2], active_member)) or 0)
		end

		local ttl = math.floor(window / 1000) + 60
		redis.call("EXPIRE", KEYS[1], ttl)
		redis.call("EXPIRE", KEYS[2], ttl)
		redis.call("EXPIRE", KEYS[3], ttl)

		return total
`

// Atomically applies a durable spend settlement once. The companion set keeps
// settlement IDs for the same lifetime as the spend counter, making retries
// safe when a network error leaves the first result ambiguous.
const applySpendSettlementScript = `
	local current = tonumber(redis.call("GET", KEYS[1]) or "0")
	if redis.call("SISMEMBER", KEYS[2], ARGV[1]) == 1 then
		return current
	end
	local count = redis.call("INCRBY", KEYS[1], ARGV[2])
	redis.call("SADD", KEYS[2], ARGV[1])
	local ttl = tonumber(ARGV[3])
	if ttl > 0 then
		redis.call("PEXPIRE", KEYS[1], ttl)
		redis.call("PEXPIRE", KEYS[2], ttl)
	end
	return count
`

// Atomically raises a spend counter to a durable ledger minimum. Unlike an
// additive settlement, replaying this after an earlier request settlement can
// never double charge the counter.
const ensureSpendFloorScript = `
	local current = tonumber(redis.call("GET", KEYS[1]) or "0")
	if redis.call("SISMEMBER", KEYS[2], ARGV[1]) == 1 then
		return current
	end
	local minimum = tonumber(ARGV[2])
	if current < minimum then
		redis.call("SET", KEYS[1], minimum)
		current = minimum
	end
	redis.call("SADD", KEYS[2], ARGV[1])
	local ttl = tonumber(ARGV[3])
	if ttl > 0 then
		redis.call("PEXPIRE", KEYS[1], ttl)
		redis.call("PEXPIRE", KEYS[2], ttl)
	end
	return current
`

var (
	slidingWindowMu  sync.Mutex
	slidingWindowSHA string
)

// SlidingWindowIncr atomically adds the current timestamp to a sliding window
// counter and returns the count of requests in the window.
func (c *Client) SlidingWindowIncr(ctx context.Context, key string, window time.Duration) (int64, error) {
	return c.SlidingWindowAdd(ctx, key, window, 1)
}

// SlidingWindowAdd atomically adds a weighted event to a sliding window counter
// and returns the weighted total in the window.
func (c *Client) SlidingWindowAdd(ctx context.Context, key string, window time.Duration, amount int64) (int64, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}
	if amount <= 0 {
		amount = 1
	}

	sha, err := c.slidingWindowScriptSHA(ctx)
	if err != nil {
		return 0, err
	}

	now := time.Now().UnixMilli()
	windowMS := int64(window / time.Millisecond)

	count, err := c.EvalSha(ctx, sha, []string{key, key + ":weights", key + ":seq"}, now, windowMS, amount).Result()
	if err != nil {
		// Fall back to plain EVAL if script not cached.
		count, err = c.Eval(ctx, slidingWindowScript, []string{key, key + ":weights", key + ":seq"}, now, windowMS, amount).Result()
		if err != nil {
			return 0, fmt.Errorf("eval sliding window: %w", err)
		}
	}

	switch v := count.(type) {
	case int64:
		return v, nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse count: %w", err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("unexpected count type: %T", count)
	}
}

func (c *Client) slidingWindowScriptSHA(ctx context.Context) (string, error) {
	slidingWindowMu.Lock()
	defer slidingWindowMu.Unlock()

	if slidingWindowSHA == "" {
		sha, err := c.ScriptLoad(ctx, slidingWindowScript).Result()
		if err != nil {
			return "", fmt.Errorf("load sliding window script: %w", err)
		}
		slidingWindowSHA = sha
	}

	return slidingWindowSHA, nil
}

// DailyIncr increments a daily counter (resets at midnight UTC).
// The key format should include the date, e.g. "quota:daily:2026-06-01:{scope}".
func (c *Client) DailyIncr(ctx context.Context, key string) (int64, error) {
	return c.DailyAdd(ctx, key, 1)
}

// DailyAdd increments a daily counter by amount (resets at midnight UTC).
// The key format should include the date, e.g. "quota:daily:2026-06-01:{scope}".
func (c *Client) DailyAdd(ctx context.Context, key string, amount int64) (int64, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}
	if amount == 0 {
		return c.GetCounter(ctx, key)
	}

	now := time.Now().UTC()
	// TTL until end of today UTC.
	ttl := time.Until(time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.UTC))

	count, err := c.IncrBy(ctx, key, amount).Result()
	if err != nil {
		return 0, fmt.Errorf("daily incr: %w", err)
	}

	// Set expiry on first increment. Negative adjustments should only happen
	// after a reservation, but still get a TTL if Redis had already evicted it.
	if count == amount || amount < 0 {
		c.Expire(ctx, key, ttl)
	}

	return count, nil
}

// MonthlyIncr increments a monthly counter (resets at end of month UTC).
func (c *Client) MonthlyIncr(ctx context.Context, key string) (int64, error) {
	return c.MonthlyAdd(ctx, key, 1)
}

// MonthlyAdd increments a monthly counter by amount (resets at end of month UTC).
func (c *Client) MonthlyAdd(ctx context.Context, key string, amount int64) (int64, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}
	if amount == 0 {
		return c.GetCounter(ctx, key)
	}

	now := time.Now().UTC()
	// TTL until end of month UTC.
	firstOfNext := time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	ttl := time.Until(firstOfNext)

	count, err := c.IncrBy(ctx, key, amount).Result()
	if err != nil {
		return 0, fmt.Errorf("monthly incr: %w", err)
	}

	if count == amount || amount < 0 {
		c.Expire(ctx, key, ttl)
	}

	return count, nil
}

// ApplySpendSettlement increments a spend counter exactly once for the given
// settlement ID. Retrying after an ambiguous Redis/network error is safe.
func (c *Client) ApplySpendSettlement(ctx context.Context, counterKey, settlementID string, amountMillicents int64, ttl time.Duration) (int64, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}
	if counterKey == "" || settlementID == "" || amountMillicents <= 0 {
		return 0, fmt.Errorf("invalid spend settlement")
	}
	ttlMillis := ttl.Milliseconds()
	if ttlMillis <= 0 {
		ttlMillis = int64(time.Hour / time.Millisecond)
	}
	count, err := c.Eval(
		ctx,
		applySpendSettlementScript,
		[]string{counterKey, counterKey + ":settlements"},
		settlementID,
		amountMillicents,
		ttlMillis,
	).Int64()
	if err != nil {
		return 0, fmt.Errorf("apply spend settlement: %w", err)
	}
	return count, nil
}

// EnsureSpendFloor raises a spend counter to minimumMillicents exactly once
// for the supplied ledger snapshot. Existing higher totals are preserved.
func (c *Client) EnsureSpendFloor(ctx context.Context, counterKey, settlementID string, minimumMillicents int64, ttl time.Duration) (int64, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}
	if counterKey == "" || settlementID == "" || minimumMillicents <= 0 {
		return 0, fmt.Errorf("invalid spend floor")
	}
	ttlMillis := ttl.Milliseconds()
	if ttlMillis <= 0 {
		ttlMillis = int64(time.Hour / time.Millisecond)
	}
	count, err := c.Eval(
		ctx,
		ensureSpendFloorScript,
		[]string{counterKey, counterKey + ":settlements"},
		settlementID,
		minimumMillicents,
		ttlMillis,
	).Int64()
	if err != nil {
		return 0, fmt.Errorf("ensure spend floor: %w", err)
	}
	return count, nil
}

// GetCounter reads the current integer value of a counter key. A missing key
// (never incremented, or expired/reset) returns 0. Disconnected clients return
// 0 without error so read-only callers degrade gracefully.
func (c *Client) GetCounter(ctx context.Context, key string) (int64, error) {
	if !c.IsConnected() {
		return 0, nil
	}
	v, err := c.Get(ctx, key).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get counter %s: %w", key, err)
	}
	return v, nil
}

// concurrencySafetyTTL bounds how long a leaked concurrency slot can persist if
// a proxy instance dies after ConcurrencyIncr but before the matching decr
// (e.g. process crash mid-stream). It is deliberately LONGER than the maximum
// legitimate slot hold — max_stream_duration defaults to 600s and a stream may
// run its full budget while holding the slot — so the safety net never reclaims
// an active slot out from under an in-flight request. Keep this comfortably
// above the largest max_stream_duration you configure.
const concurrencySafetyTTL = 15 * time.Minute

// ConcurrencyIncr increments the concurrency counter and returns the new count.
func (c *Client) ConcurrencyIncr(ctx context.Context, key string) (int64, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}

	count, err := c.Incr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("concurrency incr: %w", err)
	}

	// Refresh the safety TTL on every increment, not just the first (count == 1).
	// A busy key that never drains to zero would otherwise keep the single TTL
	// set when it first hit 1, and Redis expires the key at that absolute time
	// regardless of ongoing activity — silently resetting a still-active counter.
	// Sliding the expiry forward on each incr keeps the key alive while there is
	// traffic and lets it self-clear only after activity actually stops.
	c.Expire(ctx, key, concurrencySafetyTTL)

	return count, nil
}

// ConcurrencyDecr decrements the concurrency counter and returns the new count.
func (c *Client) ConcurrencyDecr(ctx context.Context, key string) (int64, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}

	count, err := c.Decr(ctx, key).Result()
	if err != nil {
		return 0, fmt.Errorf("concurrency decr: %w", err)
	}

	// Clean up counter when it reaches zero.
	if count <= 0 {
		c.Del(ctx, key)
	}

	return count, nil
}
