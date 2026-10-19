package providerbalance

import (
	"math"
	"sort"
	"time"
)

// ResetEvent is inferred from consecutive successful upstream observations.
// Early resets are located at first observation, not an asserted trigger time.
type ResetEvent struct {
	WindowName          string     `json:"window_name"`
	WindowSeconds       int64      `json:"window_seconds"`
	Kind                string     `json:"kind"`
	OccurredAt          time.Time  `json:"occurred_at"`
	ObservedAt          time.Time  `json:"observed_at"`
	PreviousObservedAt  time.Time  `json:"previous_observed_at"`
	PreviousUsedPercent float64    `json:"previous_used_percent"`
	UsedPercent         float64    `json:"used_percent"`
	PreviousResetsAt    *time.Time `json:"previous_resets_at,omitempty"`
	ResetsAt            *time.Time `json:"resets_at,omitempty"`
}

func ValidUsageWindow(w UsageWindow) bool {
	return w.Name != "" && w.WindowSeconds > 0 && !math.IsNaN(w.UsedPercent) && !math.IsInf(w.UsedPercent, 0) && w.UsedPercent >= 0 && w.UsedPercent <= 100
}

// ClassifyReset shares the same evidence rules between charting and accounting.
// Ignore small reset-clock corrections, ordinary downward usage corrections,
// and stale reset timestamps. A shifted deadline plus a usage drop indicates
// an early reset; it does not identify who initiated it.
func ClassifyReset(previous, current UsageWindow, previousAt, currentAt time.Time) *ResetEvent {
	if !ValidUsageWindow(previous) || !ValidUsageWindow(current) || previous.Name != current.Name || previous.WindowSeconds != current.WindowSeconds || !currentAt.After(previousAt) || previous.ResetsAt == nil {
		return nil
	}
	kind := ""
	at := currentAt
	if previous.ResetsAt.After(previousAt) && !previous.ResetsAt.After(currentAt) {
		kind, at = "scheduled", *previous.ResetsAt
	} else if previous.ResetsAt.After(currentAt) && current.ResetsAt != nil && current.ResetsAt.After(previous.ResetsAt.Add(time.Minute)) && current.UsedPercent < previous.UsedPercent {
		kind = "early"
	}
	if kind == "" {
		return nil
	}
	return &ResetEvent{WindowName: current.Name, WindowSeconds: current.WindowSeconds, Kind: kind, OccurredAt: at, ObservedAt: currentAt, PreviousObservedAt: previousAt, PreviousUsedPercent: previous.UsedPercent, UsedPercent: current.UsedPercent, PreviousResetsAt: previous.ResetsAt, ResetsAt: current.ResetsAt}
}

func DetectResets(snapshots []Snapshot) []ResetEvent {
	ordered := append([]Snapshot(nil), snapshots...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].ObservedAt.Before(ordered[j].ObservedAt) })
	type key struct {
		account, name string
		seconds       int64
	}
	type observation struct {
		window UsageWindow
		at     time.Time
	}
	previous := map[key]observation{}
	resets := []ResetEvent{}
	for _, snapshot := range ordered {
		if snapshot.Status != "success" || snapshot.BalanceKind != "usage" || snapshot.NormalizationError != "" {
			continue
		}
		for _, window := range snapshot.UsageWindows {
			if !ValidUsageWindow(window) {
				continue
			}
			k := key{snapshot.ProviderAccountID, window.Name, window.WindowSeconds}
			if prev, ok := previous[k]; ok {
				if reset := ClassifyReset(prev.window, window, prev.at, snapshot.ObservedAt); reset != nil {
					resets = append(resets, *reset)
				}
			}
			previous[k] = observation{window, snapshot.ObservedAt}
		}
	}
	return resets
}
