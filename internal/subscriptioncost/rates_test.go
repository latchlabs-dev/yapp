package subscriptioncost_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/providerbalance"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
)

type fakeStore struct {
	accounts      []subscriptioncost.Account
	usage         []subscriptioncost.Usage
	listUsage     func(time.Time, time.Time) []subscriptioncost.Usage
	snapshots     []providerbalance.Snapshot
	listSnapshots func(time.Time, time.Time) []providerbalance.Snapshot
	previous      *subscriptioncost.RateCard
	cards         []subscriptioncost.RateCard
	allocated     *subscriptioncost.RateCard
}

func (s *fakeStore) ListSubscriptionAccounts(context.Context) ([]subscriptioncost.Account, error) {
	return s.accounts, nil
}

func (s *fakeStore) ListUsage(_ context.Context, _ string, from, to time.Time) ([]subscriptioncost.Usage, error) {
	if s.listUsage != nil {
		return s.listUsage(from, to), nil
	}
	return s.usage, nil
}

func (s *fakeStore) ListUtilizationSnapshots(_ context.Context, _ string, from, to time.Time) ([]providerbalance.Snapshot, error) {
	if s.listSnapshots != nil {
		return s.listSnapshots(from, to), nil
	}
	return s.snapshots, nil
}
func (s *fakeStore) LatestFallback(context.Context, string, time.Time) (*subscriptioncost.RateCard, error) {
	return s.previous, nil
}
func (s *fakeStore) UpsertRateCards(_ context.Context, cards []subscriptioncost.RateCard) error {
	s.cards = append([]subscriptioncost.RateCard(nil), cards...)
	for i := range s.cards {
		s.cards[i].ID = int64(i + 1)
		cards[i].ID = s.cards[i].ID
	}
	return nil
}
func (s *fakeStore) AllocateRateCard(_ context.Context, card subscriptioncost.RateCard) error {
	s.allocated = &card
	return nil
}
func (s *fakeStore) ListRateCards(context.Context, string, time.Time) ([]subscriptioncost.RateCard, error) {
	return s.cards, nil
}
func (s *fakeStore) ListActiveRateCards(context.Context, time.Time) ([]subscriptioncost.RateCard, error) {
	return s.cards, nil
}

func TestCalculatorUsesProviderUtilizationDeltaIncludingDirectTraffic(t *testing.T) {
	reset := time.Date(2026, time.September, 7, 3, 30, 0, 0, time.UTC)
	store := &fakeStore{usage: []subscriptioncost.Usage{
		{Model: "gpt-5.3-codex", Tokens: 60_000_000},
		{Model: "gpt-5.4", Tokens: 40_000_000},
	}, snapshots: []providerbalance.Snapshot{
		usageSnapshot(time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC), 70, 35, reset),
		usageSnapshot(time.Date(2026, time.September, 2, 18, 0, 0, 0, time.UTC), 12, 40, reset),
		usageSnapshot(time.Date(2026, time.September, 3, 3, 0, 0, 0, time.UTC), 25, 43, reset),
	}}
	calculator := subscriptioncost.NewCalculator(store)
	rateDate := time.Date(2026, time.September, 3, 4, 0, 0, 0, time.UTC)
	cards, err := calculator.CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
	}, rateDate)
	if err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if len(cards) != 3 {
		t.Fatalf("cards = %d, want fallback plus two models", len(cards))
	}
	// The longest (weekly) provider window moved from 35% to 43%. This values
	// the interval at $200 * 8% = $16, including provider usage outside PiProxy.
	// Spread over 100M proxied tokens, the effective rate is $0.16 / 1M.
	if diff := math.Abs(cards[0].USDPerMillionTokens - 0.16); diff > 1e-9 {
		t.Fatalf("rate = %.12f, want %.12f", cards[0].USDPerMillionTokens, 0.16)
	}
	if cards[0].Model != subscriptioncost.AccountFallbackModel || cards[0].SourceTokens != 100_000_000 {
		t.Fatalf("unexpected fallback card: %#v", cards[0])
	}
	if cards[0].CalculationMethod != "utilization_delta" || cards[0].SourceUsagePercent != 8 || cards[0].AccruedCostCents != 1600 {
		t.Fatalf("unexpected utilization basis: %#v", cards[0])
	}
	wantPeriodEnd := time.Date(2026, time.September, 3, 3, 0, 0, 0, time.UTC).Add(-5 * time.Second)
	if !cards[0].SourcePeriodEnd.Equal(wantPeriodEnd) {
		t.Fatalf("source period end = %s, want settlement-safe %s", cards[0].SourcePeriodEnd, wantPeriodEnd)
	}
	if store.allocated == nil || store.allocated.ID != cards[0].ID {
		t.Fatalf("utilization card was not allocated: %#v", store.allocated)
	}
	if cards[0].SourceUsageWindow != "weekly" || cards[0].SourceUsageWindowSeconds != 7*24*60*60 {
		t.Fatalf("selected usage window = %#v", cards[0])
	}
	if cards[1].AccruedCostCents != 960 || cards[2].AccruedCostCents != 640 {
		t.Fatalf("model allocations do not reconcile: %#v", cards)
	}
	for _, card := range cards[1:] {
		if card.USDPerMillionTokens != cards[0].USDPerMillionTokens {
			t.Fatalf("model %s did not inherit blended rate", card.Model)
		}
	}
}

func usageSnapshot(observedAt time.Time, fiveHourUsed, weeklyUsed float64, weeklyReset time.Time) providerbalance.Snapshot {
	fiveHourReset := observedAt.Add(5 * time.Hour)
	return providerbalance.Snapshot{
		Status: "success", BalanceKind: "usage", ObservedAt: observedAt,
		UsageWindows: []providerbalance.UsageWindow{
			{Name: "primary", UsedPercent: fiveHourUsed, WindowSeconds: 5 * 60 * 60, ResetsAt: &fiveHourReset},
			{Name: "weekly", UsedPercent: weeklyUsed, WindowSeconds: 7 * 24 * 60 * 60, ResetsAt: &weeklyReset},
		},
	}
}

func TestCalculatorFallsBackToPreviousRate(t *testing.T) {
	rateDate := time.Date(2026, time.October, 1, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{previous: &subscriptioncost.RateCard{
		USDPerMillionTokens: 1.25,
		CalculationMethod:   "utilization_delta",
		CalculatedAt:        rateDate.Add(-24 * time.Hour),
	}}
	calculator := subscriptioncost.NewCalculator(store)
	cards, err := calculator.CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
	}, rateDate)
	if err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if len(cards) != 1 || cards[0].CalculationMethod != "previous_rate" || cards[0].USDPerMillionTokens != 1.25 {
		t.Fatalf("unexpected previous-rate card: %#v", cards)
	}
}

func TestCalculatorRejectsStalePreviousRate(t *testing.T) {
	rateDate := time.Date(2026, time.October, 4, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{previous: &subscriptioncost.RateCard{
		USDPerMillionTokens: 1.25,
		CalculationMethod:   "utilization_delta",
		CalculatedAt:        rateDate.Add(-72 * time.Hour),
	}}
	cards, err := subscriptioncost.NewCalculator(store).CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
	}, rateDate)
	if err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("stale fallback remained active: %#v", cards)
	}
}

func TestCalculatorDoesNotChainPreviousRateFallback(t *testing.T) {
	rateDate := time.Date(2026, time.October, 2, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{previous: &subscriptioncost.RateCard{
		USDPerMillionTokens: 1.25,
		CalculationMethod:   "previous_rate",
		CalculatedAt:        rateDate.Add(-24 * time.Hour),
	}}
	cards, err := subscriptioncost.NewCalculator(store).CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
	}, rateDate)
	if err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if len(cards) != 0 {
		t.Fatalf("previous-rate fallback was chained: %#v", cards)
	}
}

func TestCalculatorKeepsExistingDailyUtilizationCardStable(t *testing.T) {
	rateDate := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	store := &fakeStore{cards: []subscriptioncost.RateCard{{
		ID: 9, ProviderAccountID: "codex", RateDate: rateDate,
		Model: subscriptioncost.AccountFallbackModel, CalculationMethod: "utilization_delta",
		USDPerMillionTokens: 0.30,
	}}}
	cards, err := subscriptioncost.NewCalculator(store).CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
	}, rateDate.Add(14*time.Hour))
	if err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if len(cards) != 1 || cards[0].ID != 9 || store.allocated == nil || store.allocated.ID != 9 {
		t.Fatalf("same-day card was recalculated: %#v", cards)
	}
}

func TestScheduledCalculationUsesFixedUTCDayBoundary(t *testing.T) {
	rateDate := time.Date(2026, time.September, 3, 0, 1, 0, 0, time.UTC)
	var gotFrom, gotTo time.Time
	store := &fakeStore{
		previous: &subscriptioncost.RateCard{USDPerMillionTokens: 1, CalculationMethod: "utilization_delta", CalculatedAt: rateDate.Add(-24 * time.Hour)},
		listSnapshots: func(from, to time.Time) []providerbalance.Snapshot {
			gotFrom, gotTo = from, to
			return nil
		},
	}
	if _, err := subscriptioncost.NewCalculator(store).CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
	}, rateDate); err != nil {
		t.Fatalf("calculate: %v", err)
	}
	wantTo := time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC)
	if !gotTo.Equal(wantTo) || !gotFrom.Equal(wantTo.Add(-24*time.Hour)) {
		t.Fatalf("source query = [%s, %s], want [%s, %s]", gotFrom, gotTo, wantTo.Add(-24*time.Hour), wantTo)
	}
}

func TestScheduledCalculationContinuesAfterMiddayBootstrapWithoutOverlap(t *testing.T) {
	rateDate := time.Date(2026, time.September, 4, 0, 1, 0, 0, time.UTC)
	bootstrapObservationEnd := time.Date(2026, time.September, 3, 14, 0, 0, 1, time.UTC)
	bootstrapAllocationEnd := bootstrapObservationEnd.Add(-5 * time.Second)
	var gotFrom time.Time
	store := &fakeStore{
		cards: []subscriptioncost.RateCard{{
			ID: 8, ProviderAccountID: "codex", RateDate: time.Date(2026, time.September, 3, 0, 0, 0, 0, time.UTC),
			Model: subscriptioncost.AccountFallbackModel, CalculationMethod: "utilization_delta",
			USDPerMillionTokens: 0.3, SourceUsagePercent: 8, SourcePeriodEnd: bootstrapAllocationEnd,
		}},
		listSnapshots: func(from, _ time.Time) []providerbalance.Snapshot {
			gotFrom = from
			return nil
		},
	}
	if _, err := subscriptioncost.NewCalculator(store).CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
	}, rateDate); err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if !gotFrom.Equal(bootstrapObservationEnd) {
		t.Fatalf("source period restarted at %s, want prior observation end %s", gotFrom, bootstrapObservationEnd)
	}
}

func TestCalculatorUsesConfiguredFallbackWithoutObservableMovement(t *testing.T) {
	reset := time.Date(2026, time.September, 7, 3, 30, 0, 0, time.UTC)
	store := &fakeStore{snapshots: []providerbalance.Snapshot{
		usageSnapshot(time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC), 10, 43, reset),
		usageSnapshot(time.Date(2026, time.September, 3, 3, 0, 0, 0, time.UTC), 5, 42, reset),
	}}
	calculator := subscriptioncost.NewCalculator(store)
	cards, err := calculator.CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000, FallbackUSDPerMillion: 0.75,
	}, time.Date(2026, time.September, 3, 4, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if len(cards) != 1 || cards[0].CalculationMethod != "configured_fallback" || cards[0].USDPerMillionTokens != 0.75 {
		t.Fatalf("unexpected configured-fallback card: %#v", cards)
	}
	if cards[0].SourceUsagePercent != 0 {
		t.Fatalf("downward correction created utilization: %#v", cards[0])
	}
}

func TestCalculatorScheduledResetDoesNotChargeExpiredAllowance(t *testing.T) {
	previousReset := time.Date(2026, time.September, 3, 2, 0, 0, 0, time.UTC)
	nextReset := previousReset.Add(7 * 24 * time.Hour)
	store := &fakeStore{
		usage: []subscriptioncost.Usage{{Model: "gpt-5.4", Tokens: 16_000_000}},
		snapshots: []providerbalance.Snapshot{
			{Status: "success", BalanceKind: "usage", ObservedAt: previousReset.Add(-time.Hour), UsageWindows: []providerbalance.UsageWindow{{Name: "weekly", UsedPercent: 95, WindowSeconds: 7 * 24 * 60 * 60, ResetsAt: &previousReset}}},
			{Status: "success", BalanceKind: "usage", ObservedAt: previousReset.Add(time.Hour), UsageWindows: []providerbalance.UsageWindow{{Name: "weekly", UsedPercent: 3, WindowSeconds: 7 * 24 * 60 * 60, ResetsAt: &nextReset}}},
		},
	}
	cards, err := subscriptioncost.NewCalculator(store).CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
	}, previousReset.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if len(cards) != 2 || cards[0].SourceUsagePercent != 3 || cards[0].AccruedCostCents != 600 || cards[0].USDPerMillionTokens != 0.375 {
		t.Fatalf("unexpected reset rate card: %#v", cards)
	}
}

func TestCalculatorDoesNotDoubleCountCorrectionRebound(t *testing.T) {
	reset := time.Date(2026, time.September, 7, 3, 30, 0, 0, time.UTC)
	start := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	store := &fakeStore{
		usage: []subscriptioncost.Usage{{Model: "gpt-5.4", Tokens: 20_000_000}},
		snapshots: []providerbalance.Snapshot{
			usageSnapshot(start, 1, 20, reset),
			usageSnapshot(start.Add(time.Hour), 2, 40, reset),
			usageSnapshot(start.Add(2*time.Hour), 3, 30, reset),
			usageSnapshot(start.Add(3*time.Hour), 4, 40, reset),
		},
	}
	cards, err := subscriptioncost.NewCalculator(store).CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
	}, start.Add(4*time.Hour))
	if err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if len(cards) != 2 || cards[0].SourceUsagePercent != 20 {
		t.Fatalf("correction rebound was double counted: %#v", cards)
	}
}

func TestCalculatorDoesNotTreatStaleResetTimestampAsReset(t *testing.T) {
	start := time.Date(2026, time.September, 2, 12, 0, 0, 0, time.UTC)
	staleReset := start.Add(-time.Hour)
	store := &fakeStore{
		usage: []subscriptioncost.Usage{{Model: "gpt-5.4", Tokens: 5_000_000}},
		snapshots: []providerbalance.Snapshot{
			usageSnapshot(start, 1, 40, staleReset),
			usageSnapshot(start.Add(time.Hour), 2, 30, staleReset),
			usageSnapshot(start.Add(2*time.Hour), 3, 45, staleReset),
		},
	}
	cards, err := subscriptioncost.NewCalculator(store).CalculateAccount(context.Background(), subscriptioncost.Account{
		ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
	}, start.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("calculate: %v", err)
	}
	if len(cards) != 2 || cards[0].SourceUsagePercent != 5 {
		t.Fatalf("stale reset timestamp created false utilization: %#v", cards)
	}
}

func TestResolverUsesAccountFallbackForNewModel(t *testing.T) {
	now := time.Now()
	store := &fakeStore{cards: []subscriptioncost.RateCard{{
		ID: 7, ProviderAccountID: "codex", Model: subscriptioncost.AccountFallbackModel,
		USDPerMillionTokens: 0.75, CalculatedAt: now,
	}}}
	resolver := subscriptioncost.NewResolver(store)
	if err := resolver.Refresh(context.Background(), now); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	card, ok := resolver.Resolve("codex", "brand-new-model")
	if !ok || card.ID != 7 || card.USDPerMillionTokens != 0.75 {
		t.Fatalf("fallback resolution = %#v, %v", card, ok)
	}
}

func TestCalculatorCountsUsageAfterEarlyResetWithoutChargingDiscardedAllowance(t *testing.T) {
	start := time.Date(2026, time.September, 6, 4, 0, 0, 0, time.UTC)
	oldReset, newReset := start.Add(24*time.Hour), start.Add(7*24*time.Hour)
	snapshots := []providerbalance.Snapshot{}
	for i, used := range []float64{80, 99, 0, 14} {
		reset := &oldReset
		if i >= 2 {
			reset = &newReset
		}
		snapshots = append(snapshots, providerbalance.Snapshot{Status: "success", BalanceKind: "usage", ObservedAt: start.Add(time.Duration(i) * time.Minute), UsageWindows: []providerbalance.UsageWindow{{Name: "primary", WindowSeconds: 604800, UsedPercent: used, ResetsAt: reset}}})
	}
	store := &fakeStore{usage: []subscriptioncost.Usage{{Model: "test", Tokens: 1_000_000}}, snapshots: snapshots}
	cards, err := subscriptioncost.NewCalculator(store).CalculateAccount(context.Background(), subscriptioncost.Account{ID: "codex", Currency: "USD", MonthlyCostCents: 20000}, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(cards) != 2 || cards[0].SourceUsagePercent != 33 {
		t.Fatalf("expected 19 before + 14 after reset; cards=%+v", cards)
	}
}

func TestCalculatorRepeatedScheduledResetsCountOnlyObservedConsumption(t *testing.T) {
	start := time.Date(2026, time.September, 6, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		used []float64
		want float64
	}{
		{"unused allowance expires repeatedly", []float64{95, 0, 0, 0}, 0},
		{"usage resumes after each reset", []float64{95, 3, 7, 2}, 12},
		{"empty old window does not become fully consumed", []float64{0, 3, 0, 0}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshots := []providerbalance.Snapshot{}
			for i, used := range tc.used {
				at := start.Add(time.Duration(i) * 6 * time.Hour)
				reset := at.Add(5 * time.Hour)
				snapshots = append(snapshots, providerbalance.Snapshot{Status: "success", BalanceKind: "usage", ObservedAt: at,
					UsageWindows: []providerbalance.UsageWindow{{Name: "primary", WindowSeconds: 18000, UsedPercent: used, ResetsAt: &reset}}})
			}
			store := &fakeStore{usage: []subscriptioncost.Usage{{Model: "test", Tokens: 1_000_000}}, snapshots: snapshots}
			cards, err := subscriptioncost.NewCalculator(store).CalculateAccount(context.Background(), subscriptioncost.Account{
				ID: "codex", Currency: "USD", MonthlyCostCents: 20000,
			}, start.Add(20*time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == 0 {
				if len(cards) != 0 {
					t.Fatalf("unused allowance created charges: %+v", cards)
				}
			} else if len(cards) != 2 || cards[0].SourceUsagePercent != tc.want || cards[0].AccruedCostCents != tc.want*200 {
				t.Fatalf("expected only %v observed percentage points, got %+v", tc.want, cards)
			}
		})
	}
}
