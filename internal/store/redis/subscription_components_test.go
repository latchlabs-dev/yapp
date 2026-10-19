package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/store/redis"
)

func TestSubscriptionComponentCreditsAndOutOfOrderReplay(t *testing.T) {
	ctx := context.Background()
	c, err := redis.New(ctx, redis.Config{Address: "127.0.0.1:6379"})
	if err != nil || !c.IsConnected() {
		t.Skip("local redis unavailable")
	}
	defer c.Close()
	key := "quota:test:subscription-component"
	_ = c.Del(ctx, key, key+":components").Err()
	defer c.Del(ctx, key, key+":components")
	// Existing non-subscription spend and reservations must survive credits.
	if err = c.Set(ctx, key, 5000, time.Hour).Err(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ version, target, want int64 }{{1, 2000, 7000}, {2, 500, 5500}, {1, 2000, 5500}, {2, 500, 5500}, {3, 0, 5000}} {
		got, err := c.ReconcileSpendComponent(ctx, key, "period:1", tc.version, tc.target, time.Hour)
		if err != nil || got != tc.want {
			t.Fatalf("%+v: got %d, %v", tc, got, err)
		}
	}
}
