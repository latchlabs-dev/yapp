// Package duffel adapts the vendored duffellib workspace package to the small
// project-memory interface used by PiProxy.
package duffel

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	duffellib "duffel/pkg/duffellib"
	"duffel/pkg/duffellib/search"
	"duffel/pkg/duffellib/storage"
)

// ErrNotFound is returned when a file or directory does not exist in duffel.
var ErrNotFound = errors.New("duffel: not found")

// Config holds duffel workspace configuration.
type Config struct {
	// Root is the on-disk workspace root. This replaces the old external
	// PROXY_DUFFEL_URL HTTP dependency.
	Root string
	// BaseURL, Token, and Timeout are kept for source compatibility with older
	// tests/callers. BaseURL/Token are intentionally ignored by the library
	// adapter; configure Root instead.
	BaseURL string
	Token   string
	Timeout time.Duration
	// EnableSearch registers the workspace with qmd during startup. Search still
	// works without this if qmd already has an index.
	EnableSearch bool
	Collection   string
}

// Client is a thin adapter around duffellib.Workspace.
type Client struct {
	workspace *duffellib.Workspace
	openErr   error
}

// New creates a duffel workspace client. When Root is empty, operations return
// errors; callers should generally avoid constructing a client in that state.
func New(cfg Config) *Client {
	if cfg.Root == "" {
		return &Client{openErr: errors.New("duffel: root is required")}
	}
	ws, err := duffellib.Open(duffellib.Options{
		Root:         cfg.Root,
		Collection:   cfg.Collection,
		EnableSearch: cfg.EnableSearch,
	})
	if err != nil {
		return &Client{openErr: fmt.Errorf("duffel: open workspace: %w", err)}
	}
	return &Client{workspace: ws}
}

// FileInfo is the subset of duffel's file response the proxy consumes.
type FileInfo struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

// DirEntry is a single entry in a duffel directory listing.
type DirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"isDir"`
	Kind  string `json:"kind"`
}

// SearchResult is a single duffel search hit.
type SearchResult struct {
	Path     string  `json:"path"`
	Title    string  `json:"title"`
	Snippet  string  `json:"snippet"`
	Score    float64 `json:"score"`
	Modified string  `json:"modified_at"`
}

// SearchOpts tunes a duffel search request.
type SearchOpts struct {
	Limit          int
	MinScore       float64
	Intent         string
	CandidateLimit int
	// TenantScope restricts results to canonical projects/{tenant}--{project}/
	// paths. Callers must pass the normalized tenant from memory.SanitizeProject.
	// Empty retains the unscoped search used by private administrative tooling.
	TenantScope string
}

// ReadFile returns the content of a file. Returns ErrNotFound for missing files.
func (c *Client) ReadFile(ctx context.Context, path string) (string, error) {
	if err := c.ready(ctx); err != nil {
		return "", err
	}
	info, err := c.workspace.Read(path)
	if err != nil {
		return "", mapErr(err)
	}
	return info.Content, nil
}

// WriteFile creates or replaces a file, creating parent directories as needed.
func (c *Client) WriteFile(ctx context.Context, path, content string) error {
	if err := c.ready(ctx); err != nil {
		return err
	}
	return mapErr(c.workspace.Write(path, []byte(content)))
}

// JournalAppend appends a timestamped entry to a journal file, creating the
// journal first if it does not exist yet.
func (c *Client) JournalAppend(ctx context.Context, path, content string) error {
	if err := c.ready(ctx); err != nil {
		return err
	}
	err := c.workspace.AppendJournal(path, content)
	if !errors.Is(err, storage.ErrNotFound) {
		return mapErr(err)
	}
	if err := c.workspace.CreateJournal(path, content); err == nil {
		return nil
	}
	return mapErr(c.workspace.AppendJournal(path, content))
}

// Archive moves a file into its sibling .archive/ directory.
func (c *Client) Archive(ctx context.Context, path string) error {
	if err := c.ready(ctx); err != nil {
		return err
	}
	return mapErr(c.workspace.Archive(path))
}

// ArchiveDir moves a directory into its sibling .archive directory and
// returns the archived path relative to the workspace root.
func (c *Client) ArchiveDir(ctx context.Context, path string) (string, error) {
	if err := c.ready(ctx); err != nil {
		return "", err
	}
	archivedPath, err := c.workspace.ArchiveDir(path)
	return archivedPath, mapErr(err)
}

// ListDir lists a directory. Returns ErrNotFound for missing directories.
func (c *Client) ListDir(ctx context.Context, path string) ([]DirEntry, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	listing, err := c.workspace.List(path, false)
	if err != nil {
		return nil, mapErr(err)
	}
	entries := make([]DirEntry, 0, len(listing.Entries))
	for _, entry := range listing.Entries {
		entries = append(entries, DirEntry{
			Name:  entry.Name,
			IsDir: entry.IsDir,
			Kind:  entry.Kind,
		})
	}
	return entries, nil
}

// Search runs a hybrid (BM25 + vector) search over the workspace. If qmd is
// unavailable, fall back to a bounded lexical scan so context retrieval and the
// admin search surface degrade usefully instead of returning a hard 502.
func (c *Client) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	if opts.TenantScope != "" && !canonicalProjectSegment(opts.TenantScope) {
		return nil, errors.New("duffel: invalid tenant search scope")
	}
	limit := searchLimit(opts.Limit)
	indexLimit := opts.Limit
	if opts.TenantScope != "" {
		// The shared qmd index ranks across tenants. Overfetch a bounded set,
		// then enforce ownership before returning any snippets. A scoped lexical
		// scan fills gaps when foreign matches occupied the index's top results.
		indexLimit = 100
	}
	results, err := c.workspace.Search(search.SearchOptions{
		Query:          query,
		Limit:          indexLimit,
		MinScore:       opts.MinScore,
		Intent:         opts.Intent,
		CandidateLimit: opts.CandidateLimit,
	})
	if err == nil {
		converted := convertSearchResults(results)
		if opts.TenantScope == "" {
			return converted, nil
		}
		filtered := make([]SearchResult, 0, limit)
		seen := make(map[string]bool)
		for _, result := range converted {
			if !TenantPathMatches(result.Path, opts.TenantScope) || seen[result.Path] {
				continue
			}
			// Ignore stale index entries for deleted or archived project files.
			if _, statErr := c.workspace.Store().StatFile(result.Path); statErr != nil {
				continue
			}
			filtered = append(filtered, result)
			seen[result.Path] = true
			if len(filtered) == limit {
				return filtered, nil
			}
		}
		if fallback, fallbackErr := c.searchLexicalFallback(ctx, query, opts); fallbackErr == nil {
			for _, result := range fallback {
				if seen[result.Path] {
					continue
				}
				filtered = append(filtered, result)
				if len(filtered) == limit {
					break
				}
			}
		}
		return filtered, nil
	}

	if fallback, fallbackErr := c.searchLexicalFallback(ctx, query, opts); fallbackErr == nil {
		return fallback, nil
	}
	return nil, mapErr(err)
}

func searchLimit(limit int) int {
	if limit <= 0 {
		return 10
	}
	return min(limit, 50)
}

// TenantPathMatches is the shared ownership boundary for tenant memory search
// results. Paths must be canonical and relative, and the tenant component must
// match exactly. Unscoped legacy notes and hidden archive directories have no
// authority to enter a tenant's bundle, even if an index still returns them.
func TenantPathMatches(docPath, tenant string) bool {
	if !canonicalProjectSegment(tenant) || path.Clean(docPath) != docPath ||
		strings.HasPrefix(docPath, "/") || strings.Contains(docPath, "\\") {
		return false
	}
	parts := strings.Split(docPath, "/")
	if len(parts) < 3 || parts[0] != "projects" {
		return false
	}
	scope, project, scoped := strings.Cut(parts[1], "--")
	if !scoped || scope != tenant || !canonicalProjectSegment(project) {
		return false
	}
	for _, part := range parts[2:] {
		if strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

// Mirrors the canonical path-segment contract of memory.SanitizeProject
// without introducing an import cycle between memory and its Duffel adapter.
func canonicalProjectSegment(segment string) bool {
	if segment == "" || len(segment) > 64 || strings.Trim(segment, "-.") != segment ||
		strings.Contains(segment, "..") || strings.Contains(segment, "--") {
		return false
	}
	for _, r := range segment {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func convertSearchResults(results []search.Result) []SearchResult {
	converted := make([]SearchResult, 0, len(results))
	for _, result := range results {
		converted = append(converted, SearchResult{
			Path:     result.Path,
			Title:    result.Title,
			Snippet:  result.Snippet,
			Score:    result.Score,
			Modified: result.ModifiedAt,
		})
	}
	return converted
}

type lexicalCandidate struct {
	result SearchResult
	score  int
}

func (c *Client) searchLexicalFallback(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	terms := lexicalTerms(query)
	if len(terms) == 0 {
		return []SearchResult{}, nil
	}

	limit := searchLimit(opts.Limit)

	var candidates []lexicalCandidate
	visited := 0
	visit := func(docPath, content string) {
		score, unique := lexicalScore(docPath, content, terms)
		if score == 0 || unique == 0 {
			return
		}
		candidates = append(candidates, lexicalCandidate{
			score: score,
			result: SearchResult{
				Path:    docPath,
				Title:   lexicalTitle(docPath, content),
				Snippet: lexicalSnippet(content, terms),
				Score:   float64(score),
			},
		})
	}
	if err := c.walkSearchDocuments(ctx, opts.TenantScope, 2048, &visited, visit); err != nil {
		return nil, err
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].score == candidates[j].score {
			return candidates[i].result.Path < candidates[j].result.Path
		}
		return candidates[i].score > candidates[j].score
	})

	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	out := make([]SearchResult, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.result)
	}
	return out, nil
}

func (c *Client) walkSearchDocuments(ctx context.Context, tenant string, maxFiles int, visited *int, visit func(docPath, content string)) error {
	if tenant == "" {
		return c.walkDocuments(ctx, "", maxFiles, visited, visit)
	}
	entries, err := c.ListDir(ctx, "projects")
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		projectDir := path.Join("projects", entry.Name)
		if !entry.IsDir || !TenantPathMatches(projectDir+"/memory.md", tenant) {
			continue
		}
		// Scope the filesystem walk before reading content or spending the scan
		// budget, so another tenant cannot crowd legitimate notes out of results.
		if err := c.walkDocuments(ctx, projectDir, maxFiles, visited, func(docPath, content string) {
			if TenantPathMatches(docPath, tenant) {
				visit(docPath, content)
			}
		}); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) walkDocuments(ctx context.Context, dir string, maxFiles int, visited *int, visit func(docPath, content string)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if *visited >= maxFiles {
		return nil
	}
	entries, err := c.ListDir(ctx, dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		child := entry.Name
		if dir != "" {
			child = path.Join(dir, entry.Name)
		}
		if entry.IsDir {
			if strings.HasPrefix(entry.Name, ".") {
				continue
			}
			if err := c.walkDocuments(ctx, child, maxFiles, visited, visit); err != nil {
				return err
			}
			continue
		}
		if !isSearchableDocument(child) {
			continue
		}
		*visited++
		if *visited > maxFiles {
			return nil
		}
		content, err := c.ReadFile(ctx, child)
		if err != nil || len(content) > 256*1024 {
			continue
		}
		visit(child, content)
	}
	return nil
}

func isSearchableDocument(p string) bool {
	p = strings.ToLower(p)
	return strings.HasSuffix(p, ".md") || strings.HasSuffix(p, ".txt")
}

func lexicalTerms(query string) []string {
	seen := map[string]bool{}
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !(unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-')
	})
	terms := make([]string, 0, len(fields))
	for _, term := range fields {
		term = strings.Trim(term, "-_")
		if utf8.RuneCountInString(term) < 2 || seen[term] {
			continue
		}
		seen[term] = true
		terms = append(terms, term)
	}
	return terms
}

func lexicalScore(docPath, content string, terms []string) (score, unique int) {
	lowerPath := strings.ToLower(docPath)
	haystack := lowerPath + "\n" + strings.ToLower(content)
	for _, term := range terms {
		pathHit := strings.Contains(lowerPath, term)
		count := strings.Count(haystack, term)
		if pathHit || count > 0 {
			unique++
			score += 10
			if pathHit {
				score += 5
			}
			score += min(count, 10)
		}
	}
	return score, unique
}

func lexicalTitle(docPath, content string) string {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			return strings.TrimSpace(strings.TrimLeft(line, "#"))
		}
	}
	return path.Base(docPath)
}

func lexicalSnippet(content string, terms []string) string {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		for _, term := range terms {
			if strings.Contains(lower, term) {
				return truncateSnippet(trimmed)
			}
		}
	}
	return truncateSnippet(strings.TrimSpace(content))
}

func truncateSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	runes := []rune(s)
	if len(runes) <= 260 {
		return s
	}
	return string(runes[:260]) + "..."
}

func (c *Client) ready(ctx context.Context) error {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if c == nil {
		return errors.New("duffel: nil client")
	}
	if c.openErr != nil {
		return c.openErr
	}
	if c.workspace == nil {
		return errors.New("duffel: workspace not initialized")
	}
	return nil
}

func mapErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, storage.ErrNotFound) {
		return fmt.Errorf("duffel: %w", ErrNotFound)
	}
	return err
}
