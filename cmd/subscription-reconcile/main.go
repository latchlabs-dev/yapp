// subscription-reconcile previews or applies observed-delta historical costs.
// Paid dates are explicit. Historical mutation requires stopped proxy/admin
// writers and a subsequent counter rebuild; see subscription-accounting.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/earlgrey/piproxy/internal/store/postgres"
	redisstore "github.com/earlgrey/piproxy/internal/store/redis"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	provider := flag.String("provider", "", "provider account ID (required)")
	start := flag.String("start", "", "verified paid-period start, RFC3339")
	end := flag.String("end", "", "verified paid-period end, RFC3339")
	amount := flag.Int64("amount-cents", 0, "verified amount paid in USD cents")
	create := flag.Bool("create-period", false, "record a verified historical period")
	apply := flag.Bool("apply", false, "apply historical cost adjustments; default is preview")
	rebuild := flag.Bool("rebuild-counters", false, "replace affected active quota counters from the corrected ledger")
	maintenance := flag.Bool("writers-stopped", false, "confirm proxy/admin writers stopped and usage queues drained")
	flag.Parse()
	if *provider == "" {
		return fmt.Errorf("--provider is required")
	}
	if (*apply || *create || *rebuild) && !*maintenance {
		return fmt.Errorf("historical mutation requires --writers-stopped; stop proxy/admin and drain queues first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool, err := postgres.New(ctx, postgres.Config{DSN: os.Getenv("PROXY_POSTGRES_DSN"), MaxOpenConns: 4})
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()
	store := postgres.NewSubscriptionRateStore(pool)
	if *create {
		from, err := time.Parse(time.RFC3339, *start)
		if err != nil {
			return fmt.Errorf("invalid --start")
		}
		to, err := time.Parse(time.RFC3339, *end)
		if err != nil {
			return fmt.Errorf("invalid --end")
		}
		if *amount <= 0 || *amount > 1_000_000_000 {
			return fmt.Errorf("--amount-cents must be positive and bounded")
		}
		if err = store.CreateHistoricalBillingPeriod(ctx, subscriptioncost.Period{ProviderAccountID: *provider, Start: from, End: to, Amount: *amount * 1000}); err != nil {
			return err
		}
	}
	periods, err := store.ListBillingPeriods(ctx, *provider)
	if err != nil {
		return err
	}
	if len(periods) == 0 {
		return fmt.Errorf("no verified paid periods; record one with --create-period --start --end --amount-cents")
	}
	now := time.Now().UTC()
	for i, p := range periods {
		periods[i], err = store.ReconcileBillingPeriod(ctx, p.ID, now, *apply)
		if err != nil {
			return err
		}
	}
	if *rebuild {
		if !*apply {
			return fmt.Errorf("counter rebuild requires --apply")
		}
		c, err := redisstore.New(ctx, redisstore.Config{Address: os.Getenv("PROXY_REDIS_ADDRESS"), Password: os.Getenv("PROXY_REDIS_PASSWORD")})
		if err != nil {
			return err
		}
		defer c.Close()
		baselines, err := store.SubscriptionCounterBaselines(ctx, *provider, now)
		if err != nil {
			return err
		}
		for _, b := range baselines {
			args := []interface{}{b.Amount, b.ExpiresAt.UnixMilli()}
			fields := make([]string, 0, len(b.Components))
			for k := range b.Components {
				fields = append(fields, k)
			}
			sort.Strings(fields)
			for _, k := range fields {
				args = append(args, k, b.Components[k])
			}
			// One atomic replacement per counter; unrelated provider costs are
			// included in the ledger total. Re-running after interruption is safe.
			_, err = c.Eval(ctx, `redis.call('SET',KEYS[1],ARGV[1]);redis.call('PEXPIREAT',KEYS[1],ARGV[2]);redis.call('DEL',KEYS[2]);for i=3,#ARGV,2 do redis.call('HSET',KEYS[2],ARGV[i],ARGV[i+1]);end;if #ARGV>2 then redis.call('PEXPIREAT',KEYS[2],ARGV[2]);end;return 1`, []string{b.CounterKey, b.CounterKey + ":components"}, args...).Result()
			if err != nil {
				return err
			}
			if err = store.ClearRebuiltCounterSettlements(ctx, b.CounterKey); err != nil {
				return err
			}
		}
		fmt.Printf("rebuilt %d active quota counters from all-provider ledger totals\n", len(baselines))
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"created_period": *create, "applied": *apply, "billing_periods": periods})
}
