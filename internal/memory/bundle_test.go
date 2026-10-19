package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/metrics"
)

// fakeDuffel implements duffelReader for builder tests. The builder reads
// concurrently, so the counter must be atomic.
type fakeDuffel struct {
	files         map[string]string
	searchResults []duffel.SearchResult
	err           error
	readCount     atomic.Int32
	searchCount   atomic.Int32
	searchScope   atomic.Value
}

func (f *fakeDuffel) ReadFile(ctx context.Context, path string) (string, error) {
	f.readCount.Add(1)
	if f.err != nil {
		return "", f.err
	}
	content, ok := f.files[path]
	if !ok {
		return "", duffel.ErrNotFound
	}
	return content, nil
}

func (f *fakeDuffel) Search(ctx context.Context, q string, opts duffel.SearchOpts) ([]duffel.SearchResult, error) {
	f.searchCount.Add(1)
	f.searchScope.Store(opts.TenantScope)
	if f.err != nil {
		return nil, f.err
	}
	return f.searchResults, nil
}

const sampleJournal = `---
type: journal
---

## 2026-06-01 10:00

**[lesson]** (source: agent)

Always run make migrate-up before tests.
---

## 2026-06-02 11:30

**[mistake]** (source: distiller, conversation: abc)

Assumed quota was per-user; it is per-key.
`

func newTestBuilder(f *fakeDuffel, cfg BundleConfig) *Builder {
	return NewBuilder(f, cfg, nil, zerolog.Nop())
}

func TestBuildFullBundle(t *testing.T) {
	f := &fakeDuffel{
		files: map[string]string{
			"projects/tenant-a--demo/memory.md":  "# Memory\n\nUse pnpm not npm.",
			"projects/tenant-a--demo/lessons.md": sampleJournal,
		},
		searchResults: []duffel.SearchResult{
			{Path: "projects/tenant-a--related/auth.md", Title: "Auth Notes", Snippet: "tokens rotate daily"},
			{Path: "projects/tenant-a--demo/memory.md", Title: "self", Snippet: "should be filtered"},
		},
	}
	b := newTestBuilder(f, BundleConfig{})

	bundle := b.Build(context.Background(), "tenant-a--demo", "auth tokens", 0, false)
	if bundle.Degraded {
		t.Fatal("bundle should not be degraded")
	}
	md := bundle.Markdown
	for _, want := range []string{
		"# Project memory: tenant-a--demo",
		"## Curated memory",
		"Use pnpm not npm.",
		"## Recent lessons",
		"per-key",
		"## Related notes",
		"Auth Notes",
		"## Recording observations",
		"curl",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("bundle missing %q\n---\n%s", want, md)
		}
	}
	if strings.Contains(md, "should be filtered") {
		t.Error("bundle should exclude the project's own memory file from search results")
	}
	if bundle.TokenEstimate <= 0 {
		t.Error("token estimate missing")
	}
}

func TestBuildHintSearchEnforcesTenantBeforeRendering(t *testing.T) {
	f := &fakeDuffel{
		files: map[string]string{
			"projects/tn__01abc--demo/memory.md": "Current project facts.",
		},
		// Deliberately ignore SearchOpts to simulate an untrusted or regressed
		// shared index. Every foreign field, including titles, must be dropped.
		searchResults: []duffel.SearchResult{
			{Path: "projects/tn__01other--demo/memory.md", Title: "FOREIGN", Snippet: "FOREIGN tenant secret"},
			{Path: "projects/tn__01abcx--demo/memory.md", Title: "FOREIGN prefix collision", Snippet: "FOREIGN"},
			{Path: "projects/TN__01ABC--demo/memory.md", Title: "FOREIGN case variant", Snippet: "FOREIGN"},
			{Path: "projects/tn__01abc--demo/../other.md", Title: "FOREIGN traversal", Snippet: "FOREIGN"},
			{Path: "projects/tn__01abc--demo/.archive/old.md", Title: "FOREIGN archived", Snippet: "FOREIGN"},
			{Path: "projects/legacy/memory.md", Title: "FOREIGN legacy", Snippet: "FOREIGN"},
			{Path: "notes/unscoped.md", Title: "FOREIGN unscoped", Snippet: "FOREIGN"},
			{Path: "purgatory/tn__01abc/user-user-1/observations.md", Title: "FOREIGN purgatory", Snippet: "FOREIGN unresolved note"},
			{Path: "projects/tn__01abc--demo/memory.md", Title: "OWN DUPLICATE", Snippet: "OWN DUPLICATE"},
			{Path: "projects/tn__01abc--other/memory.md", Title: "Related project", Snippet: "Safe tenant context"},
		},
	}
	bundle := newTestBuilder(f, BundleConfig{}).Build(context.Background(), "tn__01abc--demo", "secret", 0, false)
	if strings.Contains(bundle.Markdown, "FOREIGN") || strings.Contains(bundle.Markdown, "OWN DUPLICATE") {
		t.Fatalf("unauthorized search content entered bundle:\n%s", bundle.Markdown)
	}
	if !strings.Contains(bundle.Markdown, "Current project facts") || !strings.Contains(bundle.Markdown, "Safe tenant context") {
		t.Fatalf("legitimate project and related tenant context missing:\n%s", bundle.Markdown)
	}
	if got := f.searchScope.Load(); got != "tn__01abc" {
		t.Fatalf("search tenant scope = %v, want canonical tn__01abc", got)
	}
	if f.searchResults[0].Title != "FOREIGN" {
		t.Fatal("builder mutated the shared reader's result slice")
	}
}

func TestBuildUnscopedProjectSkipsHintSearch(t *testing.T) {
	for _, project := range []string{"demo", "TENANT-A--demo", "tenant-a--demo--ambiguous"} {
		t.Run(project, func(t *testing.T) {
			f := &fakeDuffel{
				files:         map[string]string{FilePath(project): "Direct project memory"},
				searchResults: []duffel.SearchResult{{Path: "projects/other--demo/memory.md", Snippet: "FOREIGN"}},
			}
			bundle := newTestBuilder(f, BundleConfig{}).Build(context.Background(), project, "hint", 0, false)
			if f.searchCount.Load() != 0 {
				t.Fatal("ambiguous project triggered shared workspace search")
			}
			if !strings.Contains(bundle.Markdown, "Direct project memory") || strings.Contains(bundle.Markdown, "FOREIGN") {
				t.Fatalf("direct legacy read or search isolation regressed: %s", bundle.Markdown)
			}
		})
	}
}

func TestBuildLexicalFallbackRetainsOnlySameTenantNotes(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no qmd index; exercises the real lexical scan
	c := duffel.New(duffel.Config{Root: t.TempDir()})
	ctx := context.Background()
	for path, content := range map[string]string{
		"projects/tenant-a--demo/memory.md":  "Current project facts",
		"projects/tenant-a--related/note.md": "tenantisolatedmarker SAFE related tenant note",
		"projects/tenant-b--other/note.md":   "tenantisolatedmarker FOREIGN tenant secret",
		"projects/legacy/note.md":            "tenantisolatedmarker FOREIGN legacy note",
		"notes/global.md":                    "tenantisolatedmarker FOREIGN global note",
	} {
		if err := c.WriteFile(ctx, path, content); err != nil {
			t.Fatal(err)
		}
	}
	bundle := NewBuilder(c, BundleConfig{}, nil, zerolog.Nop()).Build(ctx, "tenant-a--demo", "tenantisolatedmarker", 0, false)
	if bundle.Degraded || !strings.Contains(bundle.Markdown, "SAFE related tenant note") || strings.Contains(bundle.Markdown, "FOREIGN") {
		t.Fatalf("real fallback lost tenant isolation:\n%+v", bundle)
	}
}

func TestBuildToolInstructions(t *testing.T) {
	f := &fakeDuffel{files: map[string]string{"projects/demo/memory.md": "facts"}}
	b := newTestBuilder(f, BundleConfig{})
	bundle := b.Build(context.Background(), "demo", "", 0, true)
	if !strings.Contains(bundle.Markdown, "record_memory") {
		t.Error("tool-capable bundle should reference the record_memory tool")
	}
	if strings.Contains(bundle.Markdown, "curl") {
		t.Error("tool-capable bundle should not contain curl instructions")
	}
}

func TestBuildEmptyProject(t *testing.T) {
	f := &fakeDuffel{files: map[string]string{}}
	b := newTestBuilder(f, BundleConfig{})
	bundle := b.Build(context.Background(), "newproj", "", 0, false)
	if bundle.Degraded {
		t.Error("missing files should not degrade")
	}
	if bundle.Markdown != "" {
		t.Errorf("empty project should produce empty bundle, got: %s", bundle.Markdown)
	}
}

func TestBuildDegradedWhenDuffelDown(t *testing.T) {
	f := &fakeDuffel{err: errors.New("connection refused")}
	b := newTestBuilder(f, BundleConfig{})
	bundle := b.Build(context.Background(), "demo", "", 0, false)
	if !bundle.Degraded {
		t.Error("duffel failure should degrade")
	}
	if bundle.Markdown != "" {
		t.Error("degraded bundle should be empty")
	}
}

func TestBuildBudgetRespected(t *testing.T) {
	f := &fakeDuffel{
		files: map[string]string{
			"projects/demo/memory.md":  strings.Repeat("memory facts here. ", 2000),
			"projects/demo/lessons.md": sampleJournal,
		},
	}
	b := newTestBuilder(f, BundleConfig{})
	budget := 500
	bundle := b.Build(context.Background(), "demo", "", budget, false)
	// Allow slack for headers/markers, but the truncation must bite.
	if bundle.TokenEstimate > budget+150 {
		t.Errorf("bundle estimate %d exceeds budget %d", bundle.TokenEstimate, budget)
	}
	if !strings.Contains(bundle.Markdown, "truncated") {
		t.Error("oversized memory should be truncated with marker")
	}
}

func TestBuildCache(t *testing.T) {
	f := &fakeDuffel{files: map[string]string{"projects/demo/memory.md": "facts"}}
	b := newTestBuilder(f, BundleConfig{CacheTTL: time.Minute})

	first := b.Build(context.Background(), "demo", "", 0, false)
	if first.FromCache {
		t.Error("first build should miss cache")
	}
	reads := f.readCount.Load()

	second := b.Build(context.Background(), "demo", "", 0, false)
	if !second.FromCache {
		t.Error("second build should hit cache")
	}
	if f.readCount.Load() != reads {
		t.Error("cache hit should not re-read duffel")
	}

	// Hint bypasses cache.
	third := b.Build(context.Background(), "demo", "some hint", 0, false)
	if third.FromCache {
		t.Error("hinted build should bypass cache")
	}
	if f.readCount.Load() == reads {
		t.Error("hinted build should re-read duffel")
	}
}

func TestBuildSuppressesContentBelowNoiseFloor(t *testing.T) {
	f := &fakeDuffel{files: map[string]string{
		"projects/fresh/memory.md": "tiny",
	}}
	b := NewBuilder(f, BundleConfig{MinContentTokens: 80}, nil, zerolog.Nop())
	if got := b.Build(context.Background(), "fresh", "", 0, false); got.Markdown != "" {
		t.Errorf("expected empty bundle, got:\n%s", got.Markdown)
	}
}

func TestBuildDoesNotInjectLegacyMetaPrompt(t *testing.T) {
	f := &fakeDuffel{files: map[string]string{
		"meta/prompt.md":          "General model-behavior coaching must not be injected.",
		"projects/demo/memory.md": "# Distilled project knowledge\n\nUse pnpm for this repository.",
	}}
	b := newTestBuilder(f, BundleConfig{})
	bundle := b.Build(context.Background(), "demo", "", 0, true)
	if strings.Contains(bundle.Markdown, "model-behavior coaching") || strings.Contains(bundle.Markdown, "Working guidance") {
		t.Errorf("legacy meta prompt leaked into bundle:\n%s", bundle.Markdown)
	}
	if !strings.Contains(bundle.Markdown, "Use pnpm") || !strings.Contains(bundle.Markdown, "## Recording observations") {
		t.Errorf("bundle should retain distilled knowledge and memory instructions:\n%s", bundle.Markdown)
	}
}

func TestParseJournalEntries(t *testing.T) {
	entries := ParseJournalEntries(sampleJournal)
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if entries[0].Timestamp != "2026-06-01 10:00" {
		t.Errorf("timestamp = %q", entries[0].Timestamp)
	}
	if !strings.Contains(entries[1].Content, "per-key") {
		t.Errorf("content = %q", entries[1].Content)
	}
	if ParseJournalEntries("") != nil {
		t.Error("empty journal should parse to nil")
	}
}

// TestCacheCapEvicts verifies M5: the bundle cache is bounded and evicts the
// oldest-expiring entry once full, while keeping a still-valid recent entry.
func TestCacheCapEvicts(t *testing.T) {
	f := &fakeDuffel{files: map[string]string{
		// Real content so bundles are non-degraded and get cached.
		"projects/p/memory.md": "# Memory\n\nSome durable curated content for the project.",
	}}
	const maxEntries = 4
	b := NewBuilder(f, BundleConfig{CacheMaxEntries: maxEntries, CacheTTL: time.Hour}, nil, zerolog.Nop())

	// Insert one entry, then mark it as expiring far in the future so it is the
	// freshest (latest-expiring) and must survive eviction.
	keepProject := "keep"
	b.Build(context.Background(), keepProject, "", 0, false)
	keepKey := fmt.Sprintf("%s|%d|%t", keepProject, b.cfg.BudgetTokens, false)
	b.mu.Lock()
	keep := b.cache[keepKey]
	keep.expires = time.Now().Add(100 * time.Hour) // latest-expiring => never evicted
	b.cache[keepKey] = keep
	b.mu.Unlock()

	// Insert well over the cap of distinct projects; each is cached with the
	// default (sooner) TTL, so they evict each other but never the keep entry.
	for i := 0; i < maxEntries*3; i++ {
		f.files[fmt.Sprintf("projects/proj%d/memory.md", i)] = "# Memory\n\nDurable content."
		b.Build(context.Background(), fmt.Sprintf("proj%d", i), "", 0, false)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cache) > maxEntries {
		t.Fatalf("cache size = %d, want <= %d", len(b.cache), maxEntries)
	}
	if _, ok := b.cache[keepKey]; !ok {
		t.Error("latest-expiring (keep) entry should survive eviction")
	}
}

// TestCacheCapEvictsExpired verifies expired entries are dropped opportunistically
// on insert so the cap doesn't prematurely evict live entries.
func TestCacheCapEvictsExpired(t *testing.T) {
	f := &fakeDuffel{files: map[string]string{
		"projects/p/memory.md": "# Memory\n\nDurable content.",
	}}
	b := NewBuilder(f, BundleConfig{CacheMaxEntries: 100, CacheTTL: time.Hour}, nil, zerolog.Nop())

	// Seed an already-expired entry directly.
	b.mu.Lock()
	b.cache["stale|0|false"] = cachedBundle{expires: time.Now().Add(-time.Minute)}
	b.mu.Unlock()

	b.Build(context.Background(), "p", "", 0, false)

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.cache["stale|0|false"]; ok {
		t.Error("expired entry should have been pruned on insert")
	}
}

// TestDegradedBundleMetric verifies M6: a degraded bundle (duffel unhealthy)
// increments the degraded counter, and a healthy build does not.
func TestDegradedBundleMetric(t *testing.T) {
	reg := metrics.NewRegistry()
	m := metrics.NewMemoryMetrics(reg)

	// Unhealthy duffel: a non-ErrNotFound error => degraded bundle.
	bad := &fakeDuffel{err: errors.New("connection refused")}
	bb := NewBuilder(bad, BundleConfig{}, m, zerolog.Nop())
	bundle := bb.Build(context.Background(), "demo", "", 0, false)
	if !bundle.Degraded {
		t.Fatal("expected degraded bundle")
	}
	if got := gatherCounter(t, reg, "corp_proxy_memory_degraded_bundles_total", "unavailable"); got != 1 {
		t.Fatalf("degraded counter = %v, want 1", got)
	}

	// Healthy duffel must not increment the counter.
	good := &fakeDuffel{files: map[string]string{
		"projects/demo/memory.md": "# Memory\n\nReal content.",
	}}
	gb := NewBuilder(good, BundleConfig{}, m, zerolog.Nop())
	if gb.Build(context.Background(), "demo", "", 0, false).Degraded {
		t.Fatal("healthy build should not be degraded")
	}
	if got := gatherCounter(t, reg, "corp_proxy_memory_degraded_bundles_total", "unavailable"); got != 1 {
		t.Fatalf("degraded counter = %v after healthy build, want still 1", got)
	}
}

// gatherCounter reads a single labeled counter value out of the registry. It
// returns 0 when the metric/label combination has not been observed yet.
func gatherCounter(t *testing.T, reg *metrics.Registry, name, reasonLabel string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, metric := range mf.GetMetric() {
			for _, lp := range metric.GetLabel() {
				if lp.GetName() == "reason" && lp.GetValue() == reasonLabel {
					return metric.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
