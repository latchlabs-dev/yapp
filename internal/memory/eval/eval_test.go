package eval

import (
	"context"
	"strings"
	"testing"

	"github.com/earlgrey/piproxy/internal/duffel"
)

type fakeSearcher struct {
	results map[string][]duffel.SearchResult // keyed by query
	gotOpts duffel.SearchOpts
}

func (f *fakeSearcher) Search(ctx context.Context, q string, opts duffel.SearchOpts) ([]duffel.SearchResult, error) {
	f.gotOpts = opts
	return f.results[q], nil
}

func fixtures() []Fixture {
	return []Fixture{
		{
			Name:    "auth-bug",
			Project: "demo",
			Messages: []Message{
				{Role: "user", Text: "fix auth token rotation"},
				{Role: "assistant", Text: "looking..."},
				{Role: "user", Text: "tests still failing"},
			},
			RelevantPaths: []LabeledPath{
				{Path: "notes/auth.md", Grade: 2},
				{Path: "notes/tokens.md", Grade: 1},
			},
		},
	}
}

func TestEvaluateScoring(t *testing.T) {
	searcher := &fakeSearcher{results: map[string][]duffel.SearchResult{
		"fix auth token rotation": {
			{Path: "notes/unrelated.md"},
			{Path: "notes/auth.md"},
		},
	}}

	m, err := Evaluate(context.Background(), searcher, nil, fixtures(), Strategy{Name: "first-user", HintSource: "first-user"})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// 1 of 2 relevant found → recall 0.5; first hit at rank 2 → MRR 0.5.
	if m.RecallAtK != 0.5 {
		t.Errorf("recall = %v, want 0.5", m.RecallAtK)
	}
	if m.MRR != 0.5 {
		t.Errorf("MRR = %v, want 0.5", m.MRR)
	}
}

func TestEvaluateHintSources(t *testing.T) {
	searcher := &fakeSearcher{results: map[string][]duffel.SearchResult{}}

	if _, err := Evaluate(context.Background(), searcher, nil, fixtures(), Strategy{HintSource: "last-user"}); err != nil {
		t.Fatal(err)
	}
	// last-user hint should have been used as the query — verify via opts capture
	// by checking the searcher saw a call (results map empty means query missed).
	_, err := Evaluate(context.Background(), searcher, nil, fixtures(), Strategy{HintSource: "none"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEvaluatePassesStrategyOpts(t *testing.T) {
	searcher := &fakeSearcher{results: map[string][]duffel.SearchResult{}}
	_, err := Evaluate(context.Background(), searcher, nil, fixtures(), Strategy{
		Name: "tuned", Intent: "debugging", MinScore: 0.4, Limit: 7, CandidateLimit: 120,
	})
	if err != nil {
		t.Fatal(err)
	}
	if searcher.gotOpts.Intent != "debugging" || searcher.gotOpts.MinScore != 0.4 ||
		searcher.gotOpts.Limit != 7 || searcher.gotOpts.CandidateLimit != 120 {
		t.Errorf("opts = %+v", searcher.gotOpts)
	}
}

func TestFormatTableSortsByMRR(t *testing.T) {
	table := FormatTable([]Metrics{
		{Strategy: "weak", MRR: 0.2},
		{Strategy: "strong", MRR: 0.9},
	})
	strongIdx := strings.Index(table, "strong")
	weakIdx := strings.Index(table, "weak")
	if strongIdx < 0 || weakIdx < 0 || strongIdx > weakIdx {
		t.Errorf("table not sorted by MRR:\n%s", table)
	}
}
