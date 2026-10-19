// Package eval provides a replay-based evaluation harness for the project
// memory retrieval pipeline. It runs labeled fixtures (real captured sessions
// with hand-graded relevant duffel paths) against a live duffel instance
// under different retrieval strategies and reports ranking metrics, so search
// parameters (intent, min_score, limits, hint source) are chosen from data
// instead of guesswork.
package eval

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/memory"
)

// Fixture is one replayable session with relevance labels.
type Fixture struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	// Messages from the captured session (role + text), used to derive hints.
	Messages []Message `json:"messages"`
	// SuggestedHint optionally overrides hint derivation.
	SuggestedHint string `json:"suggested_hint,omitempty"`
	// RelevantPaths are hand-labeled duffel paths with grades:
	// 2 = highly relevant, 1 = somewhat relevant, 0 = irrelevant (explicit negative).
	RelevantPaths []LabeledPath `json:"relevant_paths"`
}

// Message is a single turn from a captured session.
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// LabeledPath is a duffel path with a relevance grade.
type LabeledPath struct {
	Path  string `json:"path"`
	Grade int    `json:"grade"`
}

// Strategy parameterizes one retrieval configuration under test.
type Strategy struct {
	Name           string  `json:"name"`
	Intent         string  `json:"intent,omitempty"`
	MinScore       float64 `json:"min_score,omitempty"`
	Limit          int     `json:"limit,omitempty"`
	CandidateLimit int     `json:"candidate_limit,omitempty"`
	// HintSource: "first-user" | "last-user" | "none"
	HintSource   string `json:"hint_source,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// Metrics aggregates retrieval quality over all fixtures for one strategy.
type Metrics struct {
	Strategy       string
	Fixtures       int
	RecallAtK      float64 // graded recall: fraction of relevant (grade>0) paths retrieved
	MRR            float64 // mean reciprocal rank of the first relevant hit
	BundleTokens   int     // median bundle token estimate
	BudgetOverruns int     // bundles exceeding the strategy budget
	LatencyP50     time.Duration
}

// searcher abstracts the duffel search call for testing.
type searcher interface {
	Search(ctx context.Context, query string, opts duffel.SearchOpts) ([]duffel.SearchResult, error)
}

// Evaluate runs every fixture through one strategy.
func Evaluate(ctx context.Context, d searcher, builder *memory.Builder, fixtures []Fixture, s Strategy) (Metrics, error) {
	if s.Limit <= 0 {
		s.Limit = 5
	}
	if s.BudgetTokens <= 0 {
		s.BudgetTokens = 4000
	}

	m := Metrics{Strategy: s.Name, Fixtures: len(fixtures)}
	var recallSum, mrrSum float64
	var tokenCounts []int
	var latencies []time.Duration

	for _, f := range fixtures {
		hint := deriveHint(f, s.HintSource)

		// No hint means no related-notes search (the bundle is memory+lessons
		// only); retrieval scores then reflect what that strategy misses.
		var results []duffel.SearchResult
		var err error
		start := time.Now()
		if hint != "" {
			results, err = d.Search(ctx, hint, duffel.SearchOpts{
				Limit:          s.Limit,
				MinScore:       s.MinScore,
				Intent:         s.Intent,
				CandidateLimit: s.CandidateLimit,
			})
			if err != nil {
				return m, fmt.Errorf("fixture %s: search: %w", f.Name, err)
			}
		}
		searchLatency := time.Since(start)

		recall, mrr := scoreResults(results, f.RelevantPaths)
		recallSum += recall
		mrrSum += mrr

		// Bundle assembly metrics (token budget adherence + end-to-end latency).
		if builder != nil {
			start = time.Now()
			bundle := builder.Build(ctx, f.Project, hint, s.BudgetTokens, false)
			latencies = append(latencies, searchLatency+time.Since(start))
			tokenCounts = append(tokenCounts, bundle.TokenEstimate)
			// Allow 5% slack for headers and truncation markers.
			if bundle.TokenEstimate > s.BudgetTokens+s.BudgetTokens/20 {
				m.BudgetOverruns++
			}
		} else {
			latencies = append(latencies, searchLatency)
		}
	}

	if len(fixtures) > 0 {
		m.RecallAtK = recallSum / float64(len(fixtures))
		m.MRR = mrrSum / float64(len(fixtures))
	}
	m.BundleTokens = median(tokenCounts)
	m.LatencyP50 = medianDuration(latencies)
	return m, nil
}

// deriveHint picks the search hint from fixture messages.
func deriveHint(f Fixture, source string) string {
	if f.SuggestedHint != "" {
		return f.SuggestedHint
	}
	switch source {
	case "none":
		return ""
	case "last-user":
		for i := len(f.Messages) - 1; i >= 0; i-- {
			if f.Messages[i].Role == "user" {
				return clipHint(f.Messages[i].Text)
			}
		}
	default: // "first-user"
		for _, msg := range f.Messages {
			if msg.Role == "user" {
				return clipHint(msg.Text)
			}
		}
	}
	return ""
}

// clipHint keeps hints query-sized.
func clipHint(s string) string {
	s = strings.TrimSpace(s)
	if runes := []rune(s); len(runes) > 200 {
		return string(runes[:200])
	}
	return s
}

// scoreResults computes graded recall and reciprocal rank for one fixture.
func scoreResults(results []duffel.SearchResult, labels []LabeledPath) (recall, mrr float64) {
	relevant := map[string]bool{}
	for _, l := range labels {
		if l.Grade > 0 {
			relevant[l.Path] = true
		}
	}
	if len(relevant) == 0 {
		return 1, 1 // nothing to find — trivially perfect
	}

	found := 0
	firstRank := 0
	for rank, r := range results {
		if relevant[r.Path] {
			found++
			if firstRank == 0 {
				firstRank = rank + 1
			}
		}
	}
	recall = float64(found) / float64(len(relevant))
	if firstRank > 0 {
		mrr = 1 / float64(firstRank)
	}
	return recall, mrr
}

// FormatTable renders metrics as an aligned text table sorted by MRR.
func FormatTable(all []Metrics) string {
	sorted := make([]Metrics, len(all))
	copy(sorted, all)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].MRR > sorted[j].MRR })

	var sb strings.Builder
	fmt.Fprintf(&sb, "%-24s %8s %8s %10s %9s %10s\n",
		"STRATEGY", "RECALL", "MRR", "TOKENS", "OVERRUNS", "LATENCY")
	for _, m := range sorted {
		fmt.Fprintf(&sb, "%-24s %8.3f %8.3f %10d %9d %10s\n",
			m.Strategy, m.RecallAtK, m.MRR, m.BundleTokens, m.BudgetOverruns, m.LatencyP50.Round(time.Millisecond))
	}
	return sb.String()
}

func median(values []int) int {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]int, len(values))
	copy(sorted, values)
	sort.Ints(sorted)
	return sorted[len(sorted)/2]
}

func medianDuration(values []time.Duration) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]time.Duration, len(values))
	copy(sorted, values)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}
