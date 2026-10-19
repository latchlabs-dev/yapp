package subscriptioncost

import (
	"fmt"
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/earlgrey/piproxy/internal/providerbalance"
)

const ObservedDelta = "observed_delta"
const PendingBasis = "subscription_pending"
const ObservedBasis = "subscription_observed_delta"

// Period is an explicitly recorded paid period. Allowance deadlines never
// create periods. Money is fixed-point millicents (100,000 per USD).
type Period struct {
	ID                int64      `json:"id"`
	ProviderAccountID string     `json:"provider_account_id"`
	Start             time.Time  `json:"starts_at"`
	End               time.Time  `json:"ends_at"`
	Amount            int64      `json:"amount_millicents"`
	Allocated         int64      `json:"allocated_millicents"`
	Unattributed      int64      `json:"unattributed_millicents"`
	ConsumedPoints    float64    `json:"consumed_points"`
	Revision          int64      `json:"revision"`
	EvidenceHash      string     `json:"-"`
	ObservedThrough   *time.Time `json:"observed_through"`
}

type AllocationRequest struct {
	ID          string
	CompletedAt time.Time
	Tokens      int64
}

type Allocation struct {
	Amount int64
	Basis  string
}

type AllocationResult struct {
	Requests        map[string]Allocation
	Allocated       int64
	Unattributed    int64
	ConsumedPoints  float64
	ObservedThrough *time.Time
}

// Allocate replays non-overlapping observation intervals. Replaying the same
// evidence is deterministic; late completed records can transfer unattributed
// money without increasing the paid-period budget. No daily rate is extrapolated.
func Allocate(period Period, snapshots []providerbalance.Snapshot, requests []AllocationRequest) (AllocationResult, error) {
	r := AllocationResult{Requests: make(map[string]Allocation)}
	if period.Amount <= 0 || !period.End.After(period.Start) {
		return r, fmt.Errorf("invalid paid subscription period")
	}
	byWindow := map[string][]observedUtilizationWindow{}
	for _, s := range snapshots {
		if s.Status != "success" || s.BalanceKind != "usage" || s.NormalizationError != "" || s.ObservedAt.Before(period.Start) || !s.ObservedAt.Before(period.End) {
			continue
		}
		for _, w := range s.UsageWindows {
			if !providerbalance.ValidUsageWindow(w) {
				continue
			}
			key := fmt.Sprintf("%s/%d", w.Name, w.WindowSeconds)
			byWindow[key] = append(byWindow[key], observedUtilizationWindow{observedAt: s.ObservedAt, window: w})
		}
	}
	var selected []observedUtilizationWindow
	for _, windows := range byWindow {
		if len(windows) < 2 {
			continue
		}
		if len(selected) == 0 || windows[0].window.WindowSeconds > selected[0].window.WindowSeconds || (windows[0].window.WindowSeconds == selected[0].window.WindowSeconds && windows[0].window.Name < selected[0].window.Name) {
			selected = windows
		}
	}
	reqs := append([]AllocationRequest(nil), requests...)
	sort.Slice(reqs, func(i, j int) bool {
		if reqs[i].CompletedAt.Equal(reqs[j].CompletedAt) {
			return reqs[i].ID < reqs[j].ID
		}
		return reqs[i].CompletedAt.Before(reqs[j].CompletedAt)
	})
	for _, q := range reqs {
		r.Requests[q.ID] = Allocation{Basis: PendingBasis}
	}
	if len(selected) < 2 {
		return r, nil
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].observedAt.Before(selected[j].observedAt) })
	through := selected[len(selected)-1].observedAt
	r.ObservedThrough = &through
	highWater := selected[0].window.UsedPercent
	requestIndex := 0
	for i := 1; i < len(selected); i++ {
		p, c := selected[i-1], selected[i]
		if !c.observedAt.After(p.observedAt) {
			continue
		}
		points := 0.0
		if providerbalance.ClassifyReset(p.window, c.window, p.observedAt, c.observedAt) != nil {
			points = c.window.UsedPercent
			highWater = points
		} else if c.window.UsedPercent > highWater {
			points = c.window.UsedPercent - highWater
			highWater = c.window.UsedPercent
		}
		r.ConsumedPoints += points
		// Round the cumulative target, not each interval independently.
		target := period.Amount
		if r.ConsumedPoints < 100 {
			target = int64(math.Round(float64(period.Amount) * r.ConsumedPoints / 100))
		}
		delta := target - r.Allocated
		r.Allocated = target
		var eligible []AllocationRequest
		var tokens int64
		for requestIndex < len(reqs) && !reqs[requestIndex].CompletedAt.After(c.observedAt) {
			q := reqs[requestIndex]
			requestIndex++
			if !q.CompletedAt.After(p.observedAt) {
				continue
			}
			r.Requests[q.ID] = Allocation{Basis: ObservedBasis}
			if q.Tokens > 0 {
				if tokens > math.MaxInt64-q.Tokens {
					return r, fmt.Errorf("token weight overflow")
				}
				tokens += q.Tokens
				eligible = append(eligible, q)
			}
		}
		if tokens == 0 {
			r.Unattributed += delta
			continue
		}
		// Cumulative integer division assigns the exact rounding remainder in
		// completion/ID order and cannot create or lose money.
		var cumulative, assigned int64
		for _, q := range eligible {
			cumulative += q.Tokens
			n := new(big.Int).Mul(big.NewInt(delta), big.NewInt(cumulative))
			share := n.Quo(n, big.NewInt(tokens)).Int64()
			r.Requests[q.ID] = Allocation{Amount: share - assigned, Basis: ObservedBasis}
			assigned = share
		}
	}
	return r, nil
}
