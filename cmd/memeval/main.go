// memeval is the project-memory retrieval evaluation harness.
//
// Subcommands:
//
//	extract  — pull a captured session from Postgres into a fixture JSON file
//	           (hand-label relevant_paths afterwards)
//	seed     — load a corpus directory of markdown files into a duffel workspace
//	run      — evaluate retrieval strategies against duffel using labeled fixtures
//
// Typical workflow:
//
//	memeval extract -dsn "$DSN" -conversation-id "~/.pi/sessions/x.md" -out testdata/sessions/x.json
//	$EDITOR testdata/sessions/x.json            # add relevant_paths labels
//	memeval seed -duffel-root /tmp/piproxy-memory -corpus testdata/corpus
//	memeval run -duffel-root /tmp/piproxy-memory \
//	    -fixtures testdata/sessions -strategies strategies.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/memory"
	"github.com/earlgrey/piproxy/internal/memory/eval"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "extract":
		err = runExtract(os.Args[2:])
	case "seed":
		err = runSeed(os.Args[2:])
	case "run":
		err = runEval(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: memeval <extract|seed|run> [flags]")
}

// --- extract ---

func runExtract(args []string) error {
	fs := flag.NewFlagSet("extract", flag.ExitOnError)
	dsn := fs.String("dsn", os.Getenv("PROXY_POSTGRES_DSN"), "Postgres DSN")
	conversationID := fs.String("conversation-id", "", "conversation to extract")
	project := fs.String("project", "", "project name for the fixture")
	out := fs.String("out", "", "output fixture path (default: stdout)")
	_ = fs.Parse(args)

	if *conversationID == "" {
		return fmt.Errorf("-conversation-id is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgstore.New(ctx, pgstore.Config{DSN: *dsn, MaxOpenConns: 2, MaxIdleConns: 1})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}
	defer pool.Close()

	payloads, err := pgstore.NewConversationPayloadStore(pool).ListByConversation(ctx, *conversationID, 1)
	if err != nil {
		return err
	}
	if len(payloads) == 0 {
		return fmt.Errorf("no payloads for conversation %s", *conversationID)
	}

	fixture := eval.Fixture{
		Name:    filepath.Base(*conversationID),
		Project: *project,
		// relevant_paths is intentionally empty — label by hand.
		RelevantPaths: []eval.LabeledPath{},
	}

	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payloads[0].RequestBody, &req); err != nil {
		return fmt.Errorf("parse request body: %w", err)
	}
	for _, msg := range req.Messages {
		text := contentToText(msg.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if runes := []rune(text); len(runes) > 2000 {
			text = string(runes[:2000])
		}
		fixture.Messages = append(fixture.Messages, eval.Message{Role: msg.Role, Text: text})
	}

	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		return err
	}
	if *out == "" {
		fmt.Println(string(encoded))
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(*out, encoded, 0o600); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d messages) — now hand-label relevant_paths\n", *out, len(fixture.Messages))
	return nil
}

func contentToText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// --- seed ---

func runSeed(args []string) error {
	fs := flag.NewFlagSet("seed", flag.ExitOnError)
	duffelRoot := fs.String("duffel-root", os.Getenv("PROXY_DUFFEL_ROOT"), "duffel workspace root")
	corpus := fs.String("corpus", "testdata/corpus", "directory of files to PUT into duffel (paths preserved)")
	_ = fs.Parse(args)
	if *duffelRoot == "" {
		return fmt.Errorf("--duffel-root or PROXY_DUFFEL_ROOT is required")
	}

	client := duffel.New(duffel.Config{Root: *duffelRoot})
	ctx := context.Background()

	count := 0
	err := filepath.WalkDir(*corpus, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(*corpus, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := seedFile(ctx, client, filepath.ToSlash(rel), string(data)); err != nil {
			return fmt.Errorf("seed %s: %w", rel, err)
		}
		count++
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("seeded %d files into %s\n", count, *duffelRoot)
	return nil
}

// seedFile writes a corpus file into duffel. Journal files (type: journal
// frontmatter) cannot be PUT directly — their entries are replayed through
// the journal-append API instead (timestamps are re-stamped to now).
func seedFile(ctx context.Context, client *duffel.Client, path, content string) error {
	if !strings.HasPrefix(content, "---\n") || !strings.Contains(content, "type: journal") {
		return client.WriteFile(ctx, path, content)
	}
	for _, entry := range memory.ParseJournalEntries(content) {
		if strings.TrimSpace(entry.Content) == "" {
			continue
		}
		if err := client.JournalAppend(ctx, path, entry.Content); err != nil {
			return err
		}
	}
	return nil
}

// --- run ---

func runEval(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	duffelRoot := fs.String("duffel-root", os.Getenv("PROXY_DUFFEL_ROOT"), "duffel workspace root")
	fixturesDir := fs.String("fixtures", "testdata/sessions", "directory of labeled fixture JSON files")
	strategiesPath := fs.String("strategies", "", "JSON file with []Strategy (default: built-in sweep)")
	_ = fs.Parse(args)
	if *duffelRoot == "" {
		return fmt.Errorf("--duffel-root or PROXY_DUFFEL_ROOT is required")
	}

	fixtures, err := loadFixtures(*fixturesDir)
	if err != nil {
		return err
	}
	if len(fixtures) == 0 {
		return fmt.Errorf("no fixtures in %s", *fixturesDir)
	}

	strategies := defaultStrategies()
	if *strategiesPath != "" {
		data, err := os.ReadFile(*strategiesPath)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, &strategies); err != nil {
			return fmt.Errorf("parse strategies: %w", err)
		}
	}

	client := duffel.New(duffel.Config{Root: *duffelRoot})
	ctx := context.Background()

	results := make([]eval.Metrics, 0, len(strategies))
	for _, s := range strategies {
		// A fresh builder per strategy keeps the cache from mixing configs.
		builder := memory.NewBuilder(client, memory.BundleConfig{
			BudgetTokens:   s.BudgetTokens,
			CacheTTL:       time.Nanosecond, // effectively no caching during eval
			SearchLimit:    s.Limit,
			SearchMinScore: s.MinScore,
			SearchIntent:   s.Intent,
		}, nil, zerolog.Nop())

		m, err := eval.Evaluate(ctx, client, builder, fixtures, s)
		if err != nil {
			return fmt.Errorf("strategy %s: %w", s.Name, err)
		}
		results = append(results, m)
		fmt.Fprintf(os.Stderr, "evaluated %s\n", s.Name)
	}

	fmt.Println()
	fmt.Print(eval.FormatTable(results))
	return nil
}

func loadFixtures(dir string) ([]eval.Fixture, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	fixtures := make([]eval.Fixture, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, err
		}
		var f eval.Fixture
		if err := json.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("parse fixture %s: %w", entry.Name(), err)
		}
		fixtures = append(fixtures, f)
	}
	return fixtures, nil
}

// defaultStrategies is a parameter sweep across the knobs that matter most:
// hint source, min_score, and intent usage.
func defaultStrategies() []eval.Strategy {
	return []eval.Strategy{
		{Name: "baseline-first-user", HintSource: "first-user", Limit: 5},
		{Name: "last-user", HintSource: "last-user", Limit: 5},
		{Name: "no-hint", HintSource: "none", Limit: 5},
		{Name: "min-score-0.3", HintSource: "first-user", Limit: 5, MinScore: 0.3},
		{Name: "min-score-0.5", HintSource: "first-user", Limit: 5, MinScore: 0.5},
		{Name: "intent-troubleshoot", HintSource: "first-user", Limit: 5, Intent: "project conventions, past mistakes, troubleshooting"},
		{Name: "wide-candidates", HintSource: "first-user", Limit: 5, CandidateLimit: 120},
		{Name: "limit-10", HintSource: "first-user", Limit: 10},
	}
}
