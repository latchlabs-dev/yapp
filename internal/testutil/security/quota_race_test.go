// Package security_test verifies quota integrity under concurrent load.
package security_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/quota"
	redisstore "github.com/earlgrey/piproxy/internal/store/redis"
)

// TestQuotaNoBypassUnderConcurrency verifies that the quota enforcer
// correctly limits concurrent requests even under high concurrency.
//
// This test uses the in-memory concurrent counter (not Redis) to verify
// that the quota.Enforcer correctly tracks concurrency with atomic
// increment/decrement even when many goroutines race.
func TestQuotaNoBypassUnderConcurrency(t *testing.T) {
	// Create a quota manager with a concurrency limit of 5.
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{
		{
			ID:        "test-concurrency-limit",
			TenantID:  "test-tenant",
			ScopeType: quota.ScopeUser,
			ScopeID:   "test-user",
			Priority:  1,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests: 5,
			},
			Enabled: true,
		},
	})

	// Create enforcer without Redis (uses in-memory fallback).
	enforcer := quota.NewEnforcer(manager, &redisstore.Client{}, nil)

	ctx := &testContext{}

	// Launch 20 concurrent goroutines that all try to acquire a slot.
	var wg sync.WaitGroup
	var acquired, denied int32
	acquiredCh := make(chan struct{}, 100)

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			resv, err := enforcer.CheckAndReserve(ctx, quota.ScopeUser, "test-user", "gpt-4o", 100)
			if err != nil {
				atomic.AddInt32(&denied, 1)
				return
			}

			atomic.AddInt32(&acquired, 1)
			acquiredCh <- struct{}{}

			// Hold the slot for a short time.
			time.Sleep(50 * time.Millisecond)

			// Release.
			_ = enforcer.Complete(ctx, resv, 100)
			<-acquiredCh
		}(i)
	}

	wg.Wait()
	close(acquiredCh)

	// Verify total acquired <= limit.
	if acquired > 5 {
		t.Errorf("acquired %d slots, but limit is 5 (possible bypass)", acquired)
	}

	t.Logf("Concurrency test: acquired=%d, denied=%d, limit=5", acquired, denied)
}

// TestQuotaRaceOnSameKey verifies that the in-memory concurrency counter
// is race-free when many goroutines hit the same key simultaneously.
func TestQuotaRaceOnSameKey(t *testing.T) {
	manager := quota.NewManager()
	manager.SetPolicies([]*quota.Policy{
		{
			ID:        "test-race",
			TenantID:  "test-tenant",
			ScopeType: quota.ScopeKey,
			ScopeID:   "same-key",
			Priority:  1,
			Limits: map[quota.LimitType]int64{
				quota.LimitConcurrentRequests: 3,
			},
			Enabled: true,
		},
	})

	enforcer := quota.NewEnforcer(manager, &redisstore.Client{}, nil)
	ctx := &testContext{}

	// Rapid fire 100 acquire/release cycles.
	var maxConcurrent int64
	var active int64
	var wg sync.WaitGroup

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			resv, err := enforcer.CheckAndReserve(ctx, quota.ScopeKey, "same-key", "gpt-4o", 100)
			if err != nil {
				return
			}

			current := atomic.AddInt64(&active, 1)
			if current > atomic.LoadInt64(&maxConcurrent) {
				atomic.StoreInt64(&maxConcurrent, current)
			}

			time.Sleep(time.Millisecond)

			atomic.AddInt64(&active, -1)
			_ = enforcer.Complete(ctx, resv, 100)
		}()
	}

	wg.Wait()

	if maxConcurrent > 3 {
		t.Errorf("max concurrent was %d, limit is 3 (race condition)", maxConcurrent)
	}
	t.Logf("Race test: max concurrent = %d (limit=3)", maxConcurrent)
}

// testContext implements context.Context for quota testing.
type testContext struct{}

func (c *testContext) Deadline() (time.Time, bool)       { return time.Time{}, false }
func (c *testContext) Done() <-chan struct{}             { return nil }
func (c *testContext) Err() error                        { return nil }
func (c *testContext) Value(key interface{}) interface{} { return nil }

// Ensure time is used.
var _ = time.Second
