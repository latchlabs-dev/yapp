// Package subscriptioncost calculates and serves daily effective token prices
// for fixed-price provider subscriptions.
package subscriptioncost

import (
	"context"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/earlgrey/piproxy/internal/providerbalance"
)

const AccountFallbackModel = "*"

const (
	dailyCalculationGrace = 10 * time.Minute
	maximumFallbackAge    = 48 * time.Hour
	settlementSafetyLag   = 5 * time.Second
)

// Account is the billing configuration needed to calculate a rate card.
type Account struct {
	ID                    string
	Currency              string
	MonthlyCostCents      float64
	FallbackUSDPerMillion float64
}

// Usage is cycle-to-date usage for one model.
type Usage struct {
	Model  string
	Tokens int64
}

// RateCard is one model price effective for a UTC day. Prices are USD per one
// million total billable tokens.
type RateCard struct {
	ID                        int64      `json:"id"`
	ProviderAccountID         string     `json:"provider_account_id"`
	RateDate                  time.Time  `json:"rate_date"`
	Model                     string     `json:"model"`
	Currency                  string     `json:"currency"`
	MonthlyCostCents          float64    `json:"monthly_cost_cents"`
	AccruedCostCents          float64    `json:"accrued_cost_cents"`
	SourcePeriodStart         time.Time  `json:"source_period_start"`
	SourcePeriodEnd           time.Time  `json:"source_period_end"`
	SourceTokens              int64      `json:"source_tokens"`
	SourceUsagePercent        float64    `json:"source_usage_percent"`
	SourceUsageWindow         string     `json:"source_usage_window,omitempty"`
	SourceUsageWindowSeconds  int64      `json:"source_usage_window_seconds,omitempty"`
	SourceUsageWindowResetsAt *time.Time `json:"source_usage_window_resets_at,omitempty"`
	USDPerMillionTokens       float64    `json:"usd_per_million_tokens"`
	CalculationMethod         string     `json:"calculation_method"`
	CalculatedAt              time.Time  `json:"calculated_at"`
}

type RateResolver interface {
	Resolve(providerAccountID, model string) (RateCard, bool)
}

// Store persists cards and supplies the usage and account configuration used
// by the calculator.
type Store interface {
	ListSubscriptionAccounts(ctx context.Context) ([]Account, error)
	ListUsage(ctx context.Context, providerAccountID string, from, to time.Time) ([]Usage, error)
	ListUtilizationSnapshots(ctx context.Context, providerAccountID string, from, to time.Time) ([]providerbalance.Snapshot, error)
	LatestFallback(ctx context.Context, providerAccountID string, before time.Time) (*RateCard, error)
	UpsertRateCards(ctx context.Context, cards []RateCard) error
	AllocateRateCard(ctx context.Context, card RateCard) error
	ListRateCards(ctx context.Context, providerAccountID string, rateDate time.Time) ([]RateCard, error)
	ListActiveRateCards(ctx context.Context, rateDate time.Time) ([]RateCard, error)
}

// Calculator writes a daily rate card from the provider's observed utilization
// movement. Provider utilization includes requests made outside this proxy, so
// direct traffic raises the blended price allocated to proxied tokens.
type Calculator struct {
	store Store
}

func NewCalculator(store Store) *Calculator { return &Calculator{store: store} }

func (c *Calculator) CalculateAll(ctx context.Context, rateDate time.Time) error {
	if c == nil || c.store == nil {
		return nil
	}
	accounts, err := c.store.ListSubscriptionAccounts(ctx)
	if err != nil {
		return fmt.Errorf("list subscription accounts: %w", err)
	}
	for _, account := range accounts {
		if _, err := c.CalculateAccount(ctx, account, rateDate); err != nil {
			return fmt.Errorf("calculate subscription rate for %s: %w", account.ID, err)
		}
	}
	return nil
}

func (c *Calculator) CalculateAccount(ctx context.Context, account Account, rateDate time.Time) ([]RateCard, error) {
	if c == nil || c.store == nil || account.ID == "" || account.MonthlyCostCents <= 0 {
		return nil, nil
	}
	asOf := rateDate.UTC()
	effectiveDate := utcDay(asOf)
	currentCards, err := c.store.ListRateCards(ctx, account.ID, effectiveDate)
	if err != nil {
		return nil, err
	}
	if len(currentCards) > 0 && utcDay(currentCards[0].RateDate).Equal(effectiveDate) &&
		currentCards[0].CalculationMethod == "utilization_delta" {
		if err := c.store.AllocateRateCard(ctx, currentCards[0]); err != nil {
			return nil, err
		}
		return currentCards, nil
	}

	calculationAsOf := asOf
	if asOf.Before(effectiveDate.Add(dailyCalculationGrace)) {
		// The scheduled run is fixed to the UTC day boundary so a few seconds of
		// scheduler drift cannot change the source interval.
		calculationAsOf = effectiveDate
	}
	periodStart := calculationAsOf.Add(-24 * time.Hour)
	periodEnd := calculationAsOf
	if len(currentCards) > 0 && currentCards[0].SourceUsagePercent > 0 &&
		currentCards[0].SourcePeriodEnd.After(periodStart) && currentCards[0].SourcePeriodEnd.Before(periodEnd) {
		// A first mid-day bootstrap ends mid-day. Start the next scheduled card
		// at the provider observation boundary so daily allocations remain
		// disjoint. SourcePeriodEnd is held behind that observation to let request
		// settlement finish, so restore the lag only for the snapshot query.
		periodStart = currentCards[0].SourcePeriodEnd.Add(settlementSafetyLag)
	}
	snapshots, err := c.store.ListUtilizationSnapshots(ctx, account.ID, periodStart, periodEnd)
	if err != nil {
		return nil, err
	}
	utilization := calculateUtilization(snapshots)
	if utilization.ok {
		periodStart = utilization.periodStart
		// Complete() has a two-second bounded settlement context. Keeping a wider
		// gap behind the final provider observation guarantees any additive
		// settlement for rows included in the ledger floor was attempted first.
		periodEnd = utilization.periodEnd.Add(-settlementSafetyLag)
	}
	usageRows, err := c.store.ListUsage(ctx, account.ID, periodStart, periodEnd)
	if err != nil {
		return nil, err
	}
	var totalTokens int64
	for _, row := range usageRows {
		if row.Tokens > 0 {
			totalTokens += row.Tokens
		}
	}

	currency := account.Currency
	if currency == "" {
		currency = "USD"
	}
	allocatedCost := account.MonthlyCostCents * utilization.usedPercent / 100
	price := 0.0
	method := ""
	var fallbackSource *RateCard
	if totalTokens > 0 && allocatedCost > 0 {
		price = allocatedCost / 100 * 1_000_000 / float64(totalTokens)
		method = "utilization_delta"
	} else {
		previous, previousErr := c.store.LatestFallback(ctx, account.ID, effectiveDate)
		if previousErr != nil {
			return nil, previousErr
		}
		if previous != nil && previous.SourceUsagePercent > 0 {
			fallbackSource = previous
		}
		if previous != nil && previous.USDPerMillionTokens > 0 &&
			previous.CalculationMethod == "utilization_delta" &&
			!previous.CalculatedAt.IsZero() && calculationAsOf.Sub(previous.CalculatedAt) <= maximumFallbackAge {
			price = previous.USDPerMillionTokens
			method = "previous_rate"
		}
	}
	if price <= 0 && account.FallbackUSDPerMillion > 0 {
		price = account.FallbackUSDPerMillion
		method = "configured_fallback"
	}
	if price <= 0 {
		return nil, nil
	}
	if method != "utilization_delta" && fallbackSource != nil {
		allocatedCost = fallbackSource.AccruedCostCents
		periodStart = fallbackSource.SourcePeriodStart
		periodEnd = fallbackSource.SourcePeriodEnd
		totalTokens = fallbackSource.SourceTokens
		usageRows = nil
		utilization = utilizationResult{
			usedPercent:   fallbackSource.SourceUsagePercent,
			windowName:    fallbackSource.SourceUsageWindow,
			windowSeconds: fallbackSource.SourceUsageWindowSeconds,
			resetsAt:      fallbackSource.SourceUsageWindowResetsAt,
		}
	}

	base := RateCard{
		ProviderAccountID:         account.ID,
		RateDate:                  effectiveDate,
		Currency:                  currency,
		MonthlyCostCents:          account.MonthlyCostCents,
		AccruedCostCents:          allocatedCost,
		SourcePeriodStart:         periodStart,
		SourcePeriodEnd:           periodEnd,
		SourceUsagePercent:        utilization.usedPercent,
		SourceUsageWindow:         utilization.windowName,
		SourceUsageWindowSeconds:  utilization.windowSeconds,
		SourceUsageWindowResetsAt: utilization.resetsAt,
		USDPerMillionTokens:       price,
		CalculationMethod:         method,
	}
	fallback := base
	fallback.Model = AccountFallbackModel
	fallback.SourceTokens = totalTokens
	cards := []RateCard{fallback}
	for _, row := range usageRows {
		if row.Model == "" || row.Model == AccountFallbackModel || row.Tokens <= 0 {
			continue
		}
		card := base
		card.Model = row.Model
		card.SourceTokens = row.Tokens
		if totalTokens > 0 {
			card.AccruedCostCents = allocatedCost * float64(row.Tokens) / float64(totalTokens)
		}
		cards = append(cards, card)
	}
	if err := c.store.UpsertRateCards(ctx, cards); err != nil {
		return nil, err
	}
	if method == "utilization_delta" {
		if err := c.store.AllocateRateCard(ctx, cards[0]); err != nil {
			return nil, err
		}
	}
	return cards, nil
}

type utilizationResult struct {
	ok            bool
	usedPercent   float64
	windowName    string
	windowSeconds int64
	resetsAt      *time.Time
	periodStart   time.Time
	periodEnd     time.Time
}

type observedUtilizationWindow struct {
	observedAt time.Time
	window     providerbalance.UsageWindow
}

// calculateUtilization selects the longest allowance window that has at least
// two observations, then sums positive movement. A confirmed reset starts a
// new baseline and counts only observed usage in the new window. Expired or
// discarded allowance is not consumption. Unconfirmed downward corrections
// never create negative cost.
func calculateUtilization(snapshots []providerbalance.Snapshot) utilizationResult {
	byWindow := make(map[string][]observedUtilizationWindow)
	for _, snapshot := range snapshots {
		if snapshot.Status != "success" || snapshot.BalanceKind != "usage" || snapshot.NormalizationError != "" {
			continue
		}
		for _, window := range snapshot.UsageWindows {
			if window.Name == "" || window.WindowSeconds <= 0 || window.UsedPercent < 0 || window.UsedPercent > 100 ||
				math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) {
				continue
			}
			key := fmt.Sprintf("%s\x00%d", window.Name, window.WindowSeconds)
			byWindow[key] = append(byWindow[key], observedUtilizationWindow{observedAt: snapshot.ObservedAt.UTC(), window: window})
		}
	}

	var selected []observedUtilizationWindow
	for _, observations := range byWindow {
		if len(observations) < 2 {
			continue
		}
		if len(selected) == 0 || observations[0].window.WindowSeconds > selected[0].window.WindowSeconds ||
			(observations[0].window.WindowSeconds == selected[0].window.WindowSeconds && observations[0].window.Name < selected[0].window.Name) {
			selected = observations
		}
	}
	if len(selected) < 2 {
		return utilizationResult{}
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].observedAt.Before(selected[j].observedAt) })

	result := utilizationResult{
		ok:            true,
		windowName:    selected[0].window.Name,
		windowSeconds: selected[0].window.WindowSeconds,
		periodStart:   selected[0].observedAt,
		periodEnd:     selected[len(selected)-1].observedAt,
		resetsAt:      selected[len(selected)-1].window.ResetsAt,
	}
	highWater := selected[0].window.UsedPercent
	for i := 1; i < len(selected); i++ {
		previous := selected[i-1]
		current := selected[i]
		if reset := providerbalance.ClassifyReset(previous.window, current.window, previous.observedAt, current.observedAt); reset != nil {
			// Neither scheduled nor early resets imply the old allowance was exhausted.
			result.usedPercent += current.window.UsedPercent
			highWater = current.window.UsedPercent
			continue
		}
		if current.window.UsedPercent > highWater {
			result.usedPercent += current.window.UsedPercent - highWater
			highWater = current.window.UsedPercent
		}
	}
	return result
}

func utcDay(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

// Resolver keeps the current daily cards in memory for the request path.
type Resolver struct {
	store Store
	mu    sync.RWMutex
	rates map[string]RateCard
}

func NewResolver(store Store) *Resolver {
	return &Resolver{store: store, rates: make(map[string]RateCard)}
}

func (r *Resolver) Refresh(ctx context.Context, rateDate time.Time) error {
	if r == nil || r.store == nil {
		return nil
	}
	asOf := rateDate.UTC()
	cards, err := r.store.ListActiveRateCards(ctx, utcDay(asOf))
	if err != nil {
		return err
	}
	next := make(map[string]RateCard, len(cards))
	for _, card := range cards {
		if card.CalculatedAt.IsZero() || asOf.Sub(card.CalculatedAt) > maximumFallbackAge {
			continue
		}
		if card.CalculationMethod == "previous_rate" && !utcDay(card.RateDate).Equal(utcDay(asOf)) {
			continue
		}
		next[rateKey(card.ProviderAccountID, card.Model)] = card
	}
	r.mu.Lock()
	r.rates = next
	r.mu.Unlock()
	return nil
}

func (r *Resolver) Resolve(providerAccountID, model string) (RateCard, bool) {
	if r == nil || providerAccountID == "" {
		return RateCard{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if card, ok := r.rates[rateKey(providerAccountID, model)]; ok {
		return card, true
	}
	card, ok := r.rates[rateKey(providerAccountID, AccountFallbackModel)]
	return card, ok
}

func rateKey(accountID, model string) string { return accountID + "\x00" + model }
