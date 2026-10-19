package providerbalance

import (
	"net/url"
	"testing"
	"time"
)

func TestClassifyReset(t *testing.T) {
	at := time.Date(2026, 9, 6, 4, 34, 54, 0, time.UTC)
	oldReset := time.Date(2026, 9, 7, 3, 31, 58, 0, time.UTC)
	newReset := time.Date(2026, 9, 13, 4, 39, 24, 0, time.UTC)
	jitter := oldReset.Add(time.Second)
	stale := at.Add(-time.Hour)
	for _, tc := range []struct {
		name          string
		before, after float64
		old, next     *time.Time
		currentAt     time.Time
		kind          string
	}{
		{"actual early reset", 99, 0, &oldReset, &newReset, at.Add(5 * time.Minute), "early"},
		{"timestamp jitter", 99, 98, &oldReset, &jitter, at.Add(5 * time.Minute), ""},
		{"usage correction", 99, 98, &oldReset, &oldReset, at.Add(5 * time.Minute), ""},
		{"reschedule without usage drop", 99, 99, &oldReset, &newReset, at.Add(5 * time.Minute), ""},
		{"scheduled", 99, 2, &oldReset, &newReset, oldReset.Add(time.Minute), "scheduled"},
		{"stale timestamp", 99, 2, &stale, &stale, at.Add(5 * time.Minute), ""},
		{"no reset metadata", 99, 0, nil, nil, at.Add(5 * time.Minute), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := UsageWindow{Name: "primary", WindowSeconds: 604800, UsedPercent: tc.before, ResetsAt: tc.old}
			after := UsageWindow{Name: "primary", WindowSeconds: 604800, UsedPercent: tc.after, ResetsAt: tc.next}
			reset := ClassifyReset(before, after, at, tc.currentAt)
			if tc.kind == "" {
				if reset != nil {
					t.Fatalf("unexpected reset: %+v", reset)
				}
				return
			}
			if reset == nil || reset.Kind != tc.kind {
				t.Fatalf("expected %s, got %+v", tc.kind, reset)
			}
			if tc.kind == "early" && !reset.OccurredAt.Equal(tc.currentAt) {
				t.Fatal("early reset must use observed time")
			}
		})
	}
}

func TestDetectResetsAcrossErrorsAndOutOfOrderSnapshots(t *testing.T) {
	at := time.Now().UTC()
	oldReset, nextReset := at.Add(time.Hour), at.Add(7*24*time.Hour)
	before := Snapshot{ProviderAccountID: "a", Status: "success", BalanceKind: "usage", ObservedAt: at, UsageWindows: []UsageWindow{{Name: "primary", WindowSeconds: 604800, UsedPercent: 99, ResetsAt: &oldReset}}}
	after := Snapshot{ProviderAccountID: "a", Status: "success", BalanceKind: "usage", ObservedAt: at.Add(10 * time.Minute), UsageWindows: []UsageWindow{{Name: "primary", WindowSeconds: 604800, UsedPercent: 1, ResetsAt: &nextReset}}}
	failure := Snapshot{ProviderAccountID: "a", Status: "error", ObservedAt: at.Add(5 * time.Minute)}
	resets := DetectResets([]Snapshot{after, failure, before, after})
	if len(resets) != 1 || resets[0].PreviousUsedPercent != 99 {
		t.Fatalf("resets = %+v", resets)
	}
	after.ProviderAccountID = "other"
	if len(DetectResets([]Snapshot{before, after})) != 0 {
		t.Fatal("cross-account reset")
	}
}

func TestParseHistoryQuery(t *testing.T) {
	for _, raw := range []string{"from=nonsense", "to=2026-09-07", "limit=-1", "limit=10001", "limit=no", "from=2026-09-07T00:00:00Z&to=2026-09-06T00:00:00Z"} {
		v, _ := url.ParseQuery(raw)
		if _, err := ParseHistoryQuery(v); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	v, _ := url.ParseQuery("from=2026-09-06T00:00:00Z&to=2026-09-07T00:00:00Z&limit=10000")
	q, err := ParseHistoryQuery(v)
	if err != nil || q.Limit != 10000 || q.From == nil || q.To == nil {
		t.Fatalf("query %+v: %v", q, err)
	}
}

func TestBuildHistoryUsesBaselineAndReportsTruncation(t *testing.T) {
	at := time.Date(2026, 9, 6, 4, 30, 0, 0, time.UTC)
	oldReset, nextReset := at.Add(time.Hour), at.Add(7*24*time.Hour)
	snapshots := []Snapshot{}
	for i, used := range []float64{99, 0, 10, 15} {
		reset := &oldReset
		if i > 0 {
			reset = &nextReset
		}
		snapshots = append(snapshots, Snapshot{Status: "success", BalanceKind: "usage", ObservedAt: at.Add(time.Duration(i) * 5 * time.Minute), UsageWindows: []UsageWindow{{Name: "primary", WindowSeconds: 604800, UsedPercent: used, ResetsAt: reset}}})
	}
	from := at.Add(time.Minute)
	result := BuildHistory(snapshots, HistoryQuery{From: &from, Limit: 10})
	if len(result.ProviderBalances) != 3 || len(result.ResetEvents) != 1 || result.Truncated {
		t.Fatalf("bad boundary result: %+v", result)
	}
	result = BuildHistory(snapshots, HistoryQuery{From: &from, Limit: 2})
	if len(result.ProviderBalances) != 2 || len(result.ResetEvents) != 0 || !result.Truncated {
		t.Fatalf("bad truncated result: %+v", result)
	}
	to := at.Add(-time.Minute)
	result = BuildHistory(snapshots[:0], HistoryQuery{To: &to, Limit: 10})
	if result.ProviderBalances == nil || result.ResetEvents == nil {
		t.Fatal("empty arrays must not be null")
	}
}
