package providerbalance

import (
	"fmt"
	"net/url"
	"strconv"
	"time"
)

type HistoryQuery struct {
	From  *time.Time
	To    *time.Time
	Limit int
}

func ParseHistoryQuery(values url.Values) (HistoryQuery, error) {
	q := HistoryQuery{Limit: 96}
	for name, target := range map[string]**time.Time{"from": &q.From, "to": &q.To} {
		if raw := values.Get(name); raw != "" {
			value, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				return q, fmt.Errorf("%s must be an RFC3339 timestamp", name)
			}
			*target = &value
		}
	}
	if q.From != nil && q.To != nil && !q.From.Before(*q.To) {
		return q, fmt.Errorf("from must be before to")
	}
	if raw := values.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 10000 {
			return q, fmt.Errorf("limit must be between 1 and 10000")
		}
		q.Limit = limit
	}
	return q, nil
}

// HistoryResult excludes the preceding baseline row but uses it to identify a
// reset at the first visible observation. Truncation is explicit to callers.
type HistoryResult struct {
	ProviderBalances []Snapshot   `json:"provider_balances"`
	ResetEvents      []ResetEvent `json:"reset_events"`
	Truncated        bool         `json:"truncated"`
}

func BuildHistory(history []Snapshot, query HistoryQuery) HistoryResult {
	result := HistoryResult{ProviderBalances: []Snapshot{}, ResetEvents: []ResetEvent{}}
	for _, snapshot := range history {
		if (query.From == nil || !snapshot.ObservedAt.Before(*query.From)) && (query.To == nil || snapshot.ObservedAt.Before(*query.To)) {
			result.ProviderBalances = append(result.ProviderBalances, snapshot)
		}
	}
	result.Truncated = len(result.ProviderBalances) > query.Limit
	if result.Truncated {
		result.ProviderBalances = result.ProviderBalances[len(result.ProviderBalances)-query.Limit:]
		history = history[len(history)-query.Limit-1:]
	}
	for _, reset := range DetectResets(history) {
		if (query.From == nil || !reset.OccurredAt.Before(*query.From)) &&
			(query.To == nil || reset.ObservedAt.Before(*query.To)) &&
			(!result.Truncated || !reset.ObservedAt.Before(result.ProviderBalances[0].ObservedAt)) {
			result.ResetEvents = append(result.ResetEvents, reset)
		}
	}
	return result
}
