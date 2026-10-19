package subscriptioncost_test

import (
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/providerbalance"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
)

func TestObservedAllocationCapsFirstDayAndPreservesUnattributed(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	p := subscriptioncost.Period{Start: start, End: start.AddDate(0, 1, 0), Amount: 20_000_000}
	deadline := start.AddDate(0, 0, 7)
	snapshots := make([]providerbalance.Snapshot, 0, 7)
	for i, used := range []float64{0, 30, 60, 0, 20, 50, 100} {
		if i == 3 {
			deadline = deadline.Add(time.Hour)
		}
		reset := deadline
		snapshots = append(snapshots, providerbalance.Snapshot{Status: "success", BalanceKind: "usage", ObservedAt: start.Add(time.Duration(i) * time.Minute), UsageWindows: []providerbalance.UsageWindow{{Name: "primary", WindowSeconds: 604800, UsedPercent: used, ResetsAt: &reset}}})
	}
	requests := []subscriptioncost.AllocationRequest{{ID: "a", CompletedAt: start.Add(time.Minute), Tokens: 600}, {ID: "b", CompletedAt: start.Add(time.Minute), Tokens: 400}, {ID: "c", CompletedAt: start.Add(5 * time.Minute), Tokens: 1}}
	r, err := subscriptioncost.Allocate(p, snapshots, requests)
	if err != nil {
		t.Fatal(err)
	}
	if r.Allocated != 20_000_000 || r.Requests["a"].Amount != 3_600_000 || r.Requests["b"].Amount != 2_400_000 || r.Requests["c"].Amount != 4_000_000 || r.Unattributed != 10_000_000 {
		t.Fatalf("bad capped allocation: %+v", r)
	}
	requests = append(requests, subscriptioncost.AllocationRequest{ID: "late", CompletedAt: start.Add(2 * time.Minute), Tokens: 1})
	later, err := subscriptioncost.Allocate(p, snapshots, requests)
	if err != nil {
		t.Fatal(err)
	}
	if later.Allocated != r.Allocated || later.Unattributed != 4_000_000 || later.Requests["late"].Amount != 6_000_000 {
		t.Fatalf("late transfer spent budget twice: %+v", later)
	}
}

func TestObservedAllocationResetDiscardsUnusedAndIgnoresCorrections(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	oldReset := start.Add(4 * time.Minute)
	newReset := start.AddDate(0, 0, 7)
	p := subscriptioncost.Period{Start: start, End: newReset, Amount: 20_000_000}
	snapshots := make([]providerbalance.Snapshot, 0, 6)
	for i, used := range []float64{35, 95, 90, 95, 3, 4} {
		reset := oldReset
		if i >= 4 {
			reset = newReset
		}
		snapshots = append(snapshots, providerbalance.Snapshot{Status: "success", BalanceKind: "usage", ObservedAt: start.Add(time.Duration(i) * time.Minute), UsageWindows: []providerbalance.UsageWindow{{Name: "primary", WindowSeconds: 604800, UsedPercent: used, ResetsAt: &reset}}})
	}
	r, err := subscriptioncost.Allocate(p, snapshots, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Initial 35 is unknown; 60 observed before reset plus 4 after. The unused
	// five points and correction/rebound are never consumption.
	if r.ConsumedPoints != 64 || r.Allocated != 12_800_000 || r.Unattributed != r.Allocated {
		t.Fatalf("invented consumption: %+v", r)
	}
}

func TestObservedAllocationRoundingAndUnobservedRequests(t *testing.T) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Hour)
	p := subscriptioncost.Period{Start: start, End: end, Amount: 1}
	snapshots := []providerbalance.Snapshot{}
	for i, u := range []float64{0, 100} {
		snapshots = append(snapshots, providerbalance.Snapshot{Status: "success", BalanceKind: "usage", ObservedAt: start.Add(time.Duration(i) * time.Minute), UsageWindows: []providerbalance.UsageWindow{{Name: "primary", WindowSeconds: 604800, UsedPercent: u}}})
	}
	reqs := []subscriptioncost.AllocationRequest{{ID: "a", CompletedAt: start.Add(time.Minute), Tokens: 1}, {ID: "b", CompletedAt: start.Add(time.Minute), Tokens: 1}, {ID: "pending", CompletedAt: start.Add(2 * time.Minute), Tokens: 1}}
	r, err := subscriptioncost.Allocate(p, snapshots, reqs)
	if err != nil {
		t.Fatal(err)
	}
	if r.Requests["a"].Amount+r.Requests["b"].Amount != 1 || r.Requests["pending"].Basis != subscriptioncost.PendingBasis {
		t.Fatalf("bad rounding/boundary: %+v", r)
	}
}
