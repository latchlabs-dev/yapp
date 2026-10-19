package redis

import (
	"context"
	"fmt"
	"time"
)

const reconcileSpendComponentScript = `
local current = tonumber(redis.call('GET', KEYS[1]) or '0')
local version = tonumber(redis.call('HGET', KEYS[2], ARGV[1] .. ':version') or '0')
if tonumber(ARGV[2]) <= version then return current end
local old = tonumber(redis.call('HGET', KEYS[2], ARGV[1] .. ':amount') or '0')
local target = tonumber(ARGV[3])
local next = current + target - old
if next < 0 then return redis.error_reply('subscription component exceeds counter; reconciliation required') end
redis.call('SET', KEYS[1], next)
redis.call('HSET', KEYS[2], ARGV[1] .. ':version', ARGV[2], ARGV[1] .. ':amount', ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
return next
`

// ReconcileSpendComponent changes only one paid period's contribution. Older
// deliveries and retries are no-ops, including after a credit or transfer.
func (c *Client) ReconcileSpendComponent(ctx context.Context, counter, component string, version, target int64, ttl time.Duration) (int64, error) {
	if !c.IsConnected() {
		return 0, fmt.Errorf("redis not connected")
	}
	if counter == "" || component == "" || version <= 0 || target < 0 || ttl <= 0 {
		return 0, fmt.Errorf("invalid spend component")
	}
	return c.Eval(ctx, reconcileSpendComponentScript, []string{counter, counter + ":components"}, component, version, target, ttl.Milliseconds()).Int64()
}
