package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/metrics"
)

// BundleConfig tunes context bundle assembly.
type BundleConfig struct {
	BudgetTokens     int           // total bundle budget (default 4000)
	CacheTTL         time.Duration // per-project cache TTL (default 2m)
	SearchLimit      int           // max hint-search results (default 5)
	SearchMinScore   float64       // drop search results below this score
	SearchIntent     string        // optional fixed intent for hint searches
	RecentLessons    int           // journal entries included (default 10)
	MinContentTokens int           // noise floor: skip bundles whose real content is below this
	CacheMaxEntries  int           // hard cap on cached bundles (default 4096)
}

func (c BundleConfig) withDefaults() BundleConfig {
	if c.BudgetTokens <= 0 {
		c.BudgetTokens = 4000
	}
	if c.CacheTTL <= 0 {
		c.CacheTTL = 2 * time.Minute
	}
	if c.SearchLimit <= 0 {
		c.SearchLimit = 5
	}
	if c.RecentLessons <= 0 {
		c.RecentLessons = 10
	}
	if c.CacheMaxEntries <= 0 {
		c.CacheMaxEntries = defaultCacheMaxEntries
	}
	return c
}

// defaultCacheMaxEntries bounds the bundle cache. The cache is keyed on client
// project name (× budget × tool flag), so an unbounded map grows one entry per
// distinct project forever — a memory leak / DoS vector. We cap it and evict
// the oldest-expiring entry once full.
const defaultCacheMaxEntries = 4096

// Bundle is an assembled project context bundle.
type Bundle struct {
	Project        string
	Markdown       string
	TokenEstimate  int
	Degraded       bool // duffel was unreachable; bundle is empty
	DegradedReason string
	FromCache      bool
}

// duffelReader is the duffel client subset the builder needs.
type duffelReader interface {
	ReadFile(ctx context.Context, path string) (string, error)
	Search(ctx context.Context, query string, opts duffel.SearchOpts) ([]duffel.SearchResult, error)
}

// Builder assembles token-budgeted context bundles from duffel.
type Builder struct {
	duffel  duffelReader
	cfg     BundleConfig
	metrics *metrics.MemoryMetrics
	logger  zerolog.Logger

	mu    sync.Mutex
	cache map[string]cachedBundle
}

type cachedBundle struct {
	bundle  Bundle
	expires time.Time
}

// NewBuilder creates a bundle builder. m may be nil (metrics methods are
// nil-safe), which keeps construction in tests and eval tooling trivial.
func NewBuilder(d duffelReader, cfg BundleConfig, m *metrics.MemoryMetrics, logger zerolog.Logger) *Builder {
	return &Builder{
		duffel:  d,
		cfg:     cfg.withDefaults(),
		metrics: m,
		logger:  logger,
		cache:   make(map[string]cachedBundle),
	}
}

// Build assembles the context bundle for a project. Hint-driven requests
// bypass the cache (hints vary per session). Duffel failures degrade to an
// empty bundle — never an error — so sessions are never blocked on memory.
func (b *Builder) Build(ctx context.Context, project, hint string, budgetTokens int, agentToolAvailable bool) Bundle {
	if budgetTokens <= 0 {
		budgetTokens = b.cfg.BudgetTokens
	}

	cacheKey := fmt.Sprintf("%s|%d|%t", project, budgetTokens, agentToolAvailable)
	if hint == "" {
		b.mu.Lock()
		if entry, ok := b.cache[cacheKey]; ok && time.Now().Before(entry.expires) {
			b.mu.Unlock()
			cached := entry.bundle
			cached.FromCache = true
			return cached
		}
		b.mu.Unlock()
	}

	bundle := b.assemble(ctx, project, hint, budgetTokens, agentToolAvailable)

	if hint == "" && !bundle.Degraded {
		b.mu.Lock()
		b.evictLocked(cacheKey)
		b.cache[cacheKey] = cachedBundle{bundle: bundle, expires: time.Now().Add(b.cfg.CacheTTL)}
		b.mu.Unlock()
	}
	return bundle
}

// evictLocked keeps the cache within CacheMaxEntries before an insert. It must
// be called under b.mu. It first drops expired entries opportunistically, then,
// if the map is still at/over cap (and the key being inserted is new), evicts
// the entry with the earliest expiry. The cap is small enough that a full map
// scan under the lock is cheap and bounded.
func (b *Builder) evictLocked(incoming string) {
	now := time.Now()
	for k, entry := range b.cache {
		if now.After(entry.expires) {
			delete(b.cache, k)
		}
	}
	if _, replacing := b.cache[incoming]; replacing {
		return // overwriting an existing key doesn't grow the map
	}
	if len(b.cache) < b.cfg.CacheMaxEntries {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, entry := range b.cache {
		if oldestKey == "" || entry.expires.Before(oldest) {
			oldestKey, oldest = k, entry.expires
		}
	}
	if oldestKey != "" {
		delete(b.cache, oldestKey)
	}
}

func (b *Builder) assemble(ctx context.Context, project, hint string, budget int, agentToolAvailable bool) Bundle {
	tenant := bundleTenantScope(project)
	var (
		wg            sync.WaitGroup
		memoryDoc     string
		lessonsDoc    string
		searchResults []duffel.SearchResult
		memoryErr     error
		lessonsErr    error
	)

	wg.Go(func() {
		memoryDoc, memoryErr = b.duffel.ReadFile(ctx, FilePath(project))
	})
	wg.Go(func() {
		lessonsDoc, lessonsErr = b.duffel.ReadFile(ctx, LessonsFilePath(project))
	})
	// Unscoped legacy project directories have ambiguous ownership. Direct
	// project reads remain useful to offline tooling, but can never authorize
	// a workspace-wide hint search in a tenant-facing bundle.
	if hint != "" && tenant != "" {
		wg.Go(func() {
			results, err := b.duffel.Search(ctx, hint, duffel.SearchOpts{
				Limit:       b.cfg.SearchLimit,
				MinScore:    b.cfg.SearchMinScore,
				Intent:      b.cfg.SearchIntent,
				TenantScope: tenant,
			})
			if err != nil {
				b.logger.Debug().Err(err).Str("project", project).Msg("memory hint search failed")
				return
			}
			searchResults = results
		})
	}
	wg.Wait()

	// Missing files are normal for new projects; anything else (connection
	// refused, 5xx, auth) means duffel is unhealthy → degrade to empty.
	if unhealthy(memoryErr) || unhealthy(lessonsErr) {
		err := memoryErr
		if unhealthy(lessonsErr) {
			err = lessonsErr
		}
		b.logger.Warn().Err(err).Str("project", project).Msg("duffel unavailable; returning degraded empty bundle")
		b.metrics.IncDegraded("unavailable")
		return Bundle{Project: project, Degraded: true, DegradedReason: "unavailable"}
	}

	// Enforce ownership again here: alternate readers, tests, or a regressed
	// search backend must never be able to render foreign snippets into a
	// tenant's model context. Do not mutate the reader's shared result slice.
	filtered := make([]duffel.SearchResult, 0, len(searchResults))
	for _, r := range searchResults {
		if !duffel.TenantPathMatches(r.Path, tenant) {
			continue
		}
		if r.Path == FilePath(project) || r.Path == LessonsFilePath(project) {
			continue
		}
		filtered = append(filtered, r)
	}
	searchResults = filtered

	entries := ParseJournalEntries(lessonsDoc)
	if len(entries) > b.cfg.RecentLessons {
		entries = entries[len(entries)-b.cfg.RecentLessons:]
	}

	// Noise floor: a freshly bootstrapped project has only the memory.md
	// skeleton and an "initialized" journal marker. Injecting that costs
	// attention without informing anyone — wait until real content exists.
	if b.cfg.MinContentTokens > 0 {
		var content strings.Builder
		content.WriteString(stripFrontmatter(memoryDoc))
		for _, e := range entries {
			content.WriteString("\n" + e.Content)
		}
		for _, r := range searchResults {
			content.WriteString("\n" + r.Snippet)
		}
		if EstimateTokens(content.String()) < b.cfg.MinContentTokens {
			return Bundle{Project: project}
		}
	}

	markdown := composeBundle(project, memoryDoc, entries, searchResults, budget, agentToolAvailable)
	if markdown == "" {
		return Bundle{Project: project}
	}
	return Bundle{
		Project:       project,
		Markdown:      markdown,
		TokenEstimate: EstimateTokens(markdown),
	}
}

// ScopedProject's "--" delimiter is unique: SanitizeProject collapses dash
// runs inside both components. Require a canonical round-trip instead of
// guessing an owner for a legacy, malformed, or mixed-case project key.
func bundleTenantScope(project string) string {
	tenant, name, scoped := strings.Cut(project, "--")
	if !scoped {
		return ""
	}
	canonical, err := ScopedProject(tenant, name)
	if err != nil || canonical != project {
		return ""
	}
	return tenant
}

func unhealthy(err error) bool {
	return err != nil && !errors.Is(err, duffel.ErrNotFound)
}

// Budget shares per section. Unused share spills into the next section.
const (
	shareMemory       = 0.55
	shareLessons      = 0.25
	shareSearch       = 0.15
	shareInstructions = 0.05
)

// composeBundle renders the bundle markdown under the token budget. Returns
// "" when there is no memory at all for the project (nothing worth injecting).
func composeBundle(project, memoryDoc string, lessons []JournalEntry, results []duffel.SearchResult, budget int, agentToolAvailable bool) string {
	memoryDoc = strings.TrimSpace(stripFrontmatter(memoryDoc))
	hasContent := memoryDoc != "" || len(lessons) > 0 || len(results) > 0
	if !hasContent {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("# Project memory: " + project + "\n")
	sb.WriteString("_Background knowledge collected from previous sessions on this project. Verify against the current codebase before relying on it._\n")

	spill := 0

	// Curated memory.
	memBudget := int(float64(budget) * shareMemory)
	if memoryDoc != "" {
		sb.WriteString("\n## Curated memory\n\n")
		sb.WriteString(TruncateToTokens(memoryDoc, memBudget, false))
		sb.WriteString("\n")
		spill = max(0, memBudget-EstimateTokens(memoryDoc))
	} else {
		spill = memBudget
	}

	// Recent lessons.
	lessonsBudget := int(float64(budget)*shareLessons) + spill
	if len(lessons) > 0 {
		var lb strings.Builder
		for _, e := range lessons {
			lb.WriteString("### " + e.Timestamp + "\n" + strings.TrimSpace(e.Content) + "\n\n")
		}
		sb.WriteString("\n## Recent lessons\n\n")
		sb.WriteString(TruncateToTokens(strings.TrimSpace(lb.String()), lessonsBudget, true))
		sb.WriteString("\n")
		spill = max(0, lessonsBudget-EstimateTokens(lb.String()))
	} else {
		spill = lessonsBudget
	}

	// Related notes from hint search.
	searchBudget := int(float64(budget)*shareSearch) + spill
	if len(results) > 0 {
		var rb strings.Builder
		for _, r := range results {
			title := r.Title
			if title == "" {
				title = r.Path
			}
			rb.WriteString("- **" + title + "** (" + r.Path + "): " + strings.TrimSpace(r.Snippet) + "\n")
		}
		sb.WriteString("\n## Related notes\n\n")
		sb.WriteString(TruncateToTokens(strings.TrimSpace(rb.String()), searchBudget, false))
		sb.WriteString("\n")
	}

	// Recording instructions.
	instrBudget := int(float64(budget) * shareInstructions)
	sb.WriteString("\n## Recording observations\n\n")
	sb.WriteString(TruncateToTokens(observationInstructions(project, agentToolAvailable), instrBudget, false))
	sb.WriteString("\n")

	return sb.String()
}

func observationInstructions(project string, agentToolAvailable bool) string {
	if agentToolAvailable {
		return "When requirements or tradeoffs are unclear, ask the user before choosing. When you make a notable mistake, " +
			"learn a project-specific lesson, or the user makes or confirms a significant decision, record it with the " +
			"`record_memory` tool (kinds: mistake, lesson, decision, context). Never record an agent-inferred choice as a decision. " +
			"Keep entries short and durable — skip session-specific details."
	}
	return "When requirements or tradeoffs are unclear, ask the user before choosing. Record notable mistakes, project-specific lessons, and decisions explicitly made or confirmed by the user:\n\n" +
		"```bash\ncurl -s -X POST \"$CORP_PROXY_BASE_URL/proxy/v1/observations\" \\\n" +
		"  -H \"Authorization: Bearer $CORP_PROXY_KEY\" -H 'Content-Type: application/json' \\\n" +
		"  -d '{\"project\":\"" + project + "\",\"kind\":\"lesson\",\"content\":\"...\"}'\n```\n\n" +
		"Kinds: mistake, lesson, decision, context. Never record an agent-inferred choice as a decision. Keep entries short and durable — skip session-specific details."
}

// JournalEntry is one timestamped entry in a duffel journal file.
type JournalEntry struct {
	Timestamp string // "YYYY-MM-DD HH:MM"
	Content   string
}

// ParseJournalEntries splits a duffel journal document into entries. Duffel
// journals start with frontmatter and separate entries with "\n---\n",
// each entry beginning "## YYYY-MM-DD HH:MM".
func ParseJournalEntries(content string) []JournalEntry {
	content = stripFrontmatter(content)
	if strings.TrimSpace(content) == "" {
		return nil
	}

	entries := make([]JournalEntry, 0, strings.Count(content, "\n---\n")+1)
	for block := range strings.SplitSeq(content, "\n---\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		lines := strings.SplitN(block, "\n", 2)
		header := strings.TrimSpace(strings.TrimPrefix(lines[0], "##"))
		body := ""
		if len(lines) > 1 {
			body = strings.TrimSpace(lines[1])
		}
		if !strings.HasPrefix(lines[0], "## ") {
			// Not a timestamped entry — keep it as content with no timestamp.
			entries = append(entries, JournalEntry{Content: block})
			continue
		}
		entries = append(entries, JournalEntry{Timestamp: header, Content: body})
	}
	return entries
}

// stripFrontmatter removes a leading YAML frontmatter block.
func stripFrontmatter(content string) string {
	if !strings.HasPrefix(content, "---\n") {
		return content
	}
	_, after, found := strings.Cut(content[4:], "\n---")
	if !found {
		return content
	}
	return strings.TrimPrefix(strings.TrimPrefix(after, "\n"), "\n")
}
