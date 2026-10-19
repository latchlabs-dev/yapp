package postgres

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"encoding/json"

	"github.com/earlgrey/piproxy/internal/providerbalance"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
	"github.com/earlgrey/piproxy/internal/usage"
)

func TestSubscriptionHistoricalReconciliationAndConcurrentReplay(t *testing.T) {
	dsn := os.Getenv("PIPROXY_ACCOUNTING_TEST_DSN")
	if dsn == "" {
		t.Skip("dedicated accounting test database is not configured")
	}
	ctx := context.Background()
	pool, err := New(ctx, Config{DSN: dsn, MaxOpenConns: 6})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	provider := "prv_accounting_integration"
	start := time.Now().UTC().Add(-time.Hour)
	end := start.AddDate(0, 1, 0)
	_, err = pool.Exec(ctx, `INSERT INTO provider_accounts(id,provider,name,status,config) VALUES($1,'openai-codex','Accounting test','active','{"billing":{"mode":"subscription","currency":"USD","monthly_cost_cents":20000}}')`, provider)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM subscription_allocation_adjustments WHERE period_id IN(SELECT id FROM subscription_billing_periods WHERE provider_account_id=$1)`, provider)
		_, _ = pool.Exec(ctx, `DELETE FROM usage_events WHERE provider_account_id=$1`, provider)
		_, _ = pool.Exec(ctx, `DELETE FROM subscription_quota_components WHERE period_id IN(SELECT id FROM subscription_billing_periods WHERE provider_account_id=$1)`, provider)
		_, _ = pool.Exec(ctx, `DELETE FROM subscription_billing_periods WHERE provider_account_id=$1`, provider)
		_, _ = pool.Exec(ctx, `DELETE FROM provider_balance_snapshots WHERE provider_account_id=$1`, provider)
		_, _ = pool.Exec(ctx, `DELETE FROM provider_accounts WHERE id=$1`, provider)
	})
	store := NewSubscriptionRateStore(pool)
	usageStore := NewUsageStore(pool)
	done := start.Add(time.Minute)
	event := &usage.Event{ID: "uev_accounting_test", RequestID: "req_accounting_test", ProviderAccountID: provider, Provider: "openai-codex", Model: "test", UpstreamModel: "test", Status: "success", TotalTokens: 100, CostTotalCents: 1_000_000, CostBasis: "subscription_rate_card", StartedAt: start.Add(time.Second), CompletedAt: &done}
	if err = usageStore.InsertUsageEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	p := subscriptioncost.Period{ProviderAccountID: provider, Start: start, End: end, Amount: 20_000_000}
	if err = store.CreateBillingPeriod(ctx, p); err == nil {
		t.Fatal("ordinary creation accepted legacy costs without maintenance")
	}
	if err = store.CreateHistoricalBillingPeriod(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err = store.CreateHistoricalBillingPeriod(ctx, p); err == nil {
		t.Fatal("overlapping period accepted")
	}
	periods, err := store.ListBillingPeriods(ctx, provider)
	if err != nil {
		t.Fatal(err)
	}
	p = periods[0]
	for i, used := range []float64{35, 95, 100} {
		windows, _ := json.Marshal([]providerbalance.UsageWindow{{Name: "primary", WindowSeconds: 604800, UsedPercent: used}})
		_, err = pool.Exec(ctx, `INSERT INTO provider_balance_snapshots(provider_account_id,status,balance_kind,billing_mode,observed_at,usage_windows) VALUES($1,'success','usage','subscription',$2,$3)`, provider, start.Add(time.Duration(i)*time.Minute), windows)
		if err != nil {
			t.Fatal(err)
		}
	}
	preview, err := store.ReconcileBillingPeriod(ctx, p.ID, time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Allocated != 13_000_000 {
		t.Fatalf("bad preview: %+v", preview)
	}
	var cost float64
	_ = pool.QueryRow(ctx, `SELECT cost_total_cents::double precision FROM usage_events WHERE id=$1`, event.ID).Scan(&cost)
	if cost != 1_000_000 {
		t.Fatal("preview mutated historical cost")
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.ReconcileBillingPeriod(ctx, p.ID, time.Now(), true)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	periods, _ = store.ListBillingPeriods(ctx, provider)
	if periods[0].Revision != 1 || periods[0].Allocated != 13_000_000 || periods[0].Unattributed != 1_000_000 {
		t.Fatalf("replay duplicated budget: %+v", periods)
	}
	_ = pool.QueryRow(ctx, `SELECT cost_total_cents::double precision FROM usage_events WHERE id=$1`, event.ID).Scan(&cost)
	if cost != 12_000 {
		t.Fatalf("historical cost not credited: %v", cost)
	}
	// A delayed duplicate recorder event cannot restore the obsolete price.
	if err = usageStore.InsertUsageEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	_ = pool.QueryRow(ctx, `SELECT cost_total_cents::double precision FROM usage_events WHERE id=$1`, event.ID).Scan(&cost)
	if cost != 12_000 {
		t.Fatal("duplicate recorder delivery overwrote final cost")
	}
}
