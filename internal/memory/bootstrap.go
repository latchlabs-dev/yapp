package memory

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
)

// duffelBootstrapper is the duffel client subset Bootstrapper needs.
type duffelBootstrapper interface {
	ReadFile(ctx context.Context, path string) (string, error)
	WriteFile(ctx context.Context, path, content string) error
	JournalAppend(ctx context.Context, path, content string) error
}

// Bootstrapper creates the duffel folder structure for projects the first
// time they are seen. Projects map 1:1 to git repositories: the extension
// derives the project name from the git origin and passes the repo URL along,
// which is recorded in the scaffolded memory document.
//
// Bootstrap is idempotent and best-effort: existing files are never touched,
// failures are logged and retried on a later sighting, and callers fire it
// async so context requests are never delayed.
type Bootstrapper struct {
	duffel duffelBootstrapper
	logger zerolog.Logger

	mu   sync.Mutex
	done map[string]time.Time // project → last successful/attempted bootstrap
}

// NewBootstrapper creates a Bootstrapper.
func NewBootstrapper(d duffelBootstrapper, logger zerolog.Logger) *Bootstrapper {
	return &Bootstrapper{duffel: d, logger: logger, done: make(map[string]time.Time)}
}

// retryAfter limits how often a failed bootstrap is reattempted.
const retryAfter = 10 * time.Minute

// maxBootstrapEntries caps the done map. Successful entries are re-recorded on
// every Ensure within retryAfter, so without a bound the map grows one entry
// per distinct project name forever — an unbounded-growth / DoS vector. Evicted
// entries are cheaply re-derivable on the next sighting.
const maxBootstrapEntries = 8192

// Ensure scaffolds projects/{project}/ when it does not exist yet. Safe to
// call on every context request; after the first success it is a cheap
// map lookup. repoURL may be empty.
func (b *Bootstrapper) Ensure(ctx context.Context, project, repoURL string) {
	b.ensure(ctx, project, "", repoURL)
}

// EnsureCanonical scaffolds a stable repository-ID path and safely carries
// forward the immediately preceding tenant-scoped label path. It never reads
// old unscoped project directories, which could contain another tenant's data.
func (b *Bootstrapper) EnsureCanonical(ctx context.Context, project, legacyScopedProject, repoURL string) {
	b.ensure(ctx, project, legacyScopedProject, repoURL)
}

func (b *Bootstrapper) ensure(ctx context.Context, project, legacyScopedProject, repoURL string) {
	b.mu.Lock()
	if when, ok := b.done[project]; ok && time.Since(when) < retryAfter {
		b.mu.Unlock()
		return
	}
	// Record the attempt up front so concurrent requests don't duplicate work;
	// failures age out via retryAfter.
	b.pruneLocked(project)
	b.done[project] = time.Now()
	b.mu.Unlock()

	unlock := LockProject(project)
	defer unlock()

	// Existing memory.md means the project is already set up.
	_, err := b.duffel.ReadFile(ctx, FilePath(project))
	if err == nil {
		return
	}
	if !errors.Is(err, duffel.ErrNotFound) {
		b.logger.Debug().Err(err).Str("project", project).Msg("bootstrap probe failed; will retry")
		b.forget(project)
		return
	}

	if legacyScopedProject != "" && legacyScopedProject != project {
		migrated, migrateErr := b.copyLegacyScopedProject(ctx, legacyScopedProject, project)
		if migrateErr != nil {
			b.logger.Warn().Err(migrateErr).Str("project", project).Str("legacy_project", legacyScopedProject).Msg("bootstrap: legacy project copy failed")
			b.forget(project)
			return
		}
		if migrated {
			b.logger.Info().Str("project", project).Str("legacy_project", legacyScopedProject).Msg("tenant-scoped project memory migrated to repository identity")
			return
		}
	}

	if werr := b.duffel.WriteFile(ctx, FilePath(project), memorySkeleton(project, repoURL)); werr != nil {
		b.logger.Warn().Err(werr).Str("project", project).Msg("bootstrap: memory.md create failed")
		b.forget(project)
		return
	}
	// Seed the lessons journal so it shows up in the duffel UI immediately.
	entry := "Project memory initialized."
	if repoURL != "" {
		entry = "Project memory initialized for " + repoURL + "."
	}
	if jerr := b.duffel.JournalAppend(ctx, LessonsFilePath(project), entry); jerr != nil {
		b.logger.Warn().Err(jerr).Str("project", project).Msg("bootstrap: lessons journal create failed")
		// memory.md exists now, so don't forget — the journal is auto-created
		// by the first observation/distillation anyway.
	}

	b.logger.Info().Str("project", project).Str("repo", repoURL).Msg("project memory bootstrapped")
}

func (b *Bootstrapper) copyLegacyScopedProject(ctx context.Context, source, destination string) (bool, error) {
	memoryDoc, err := b.duffel.ReadFile(ctx, FilePath(source))
	if errors.Is(err, duffel.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, paths := range [][2]string{
		{LessonsFilePath(source), LessonsFilePath(destination)},
		{PromptReflectionsFilePath(source), PromptReflectionsFilePath(destination)},
	} {
		content, readErr := b.duffel.ReadFile(ctx, paths[0])
		if errors.Is(readErr, duffel.ErrNotFound) {
			continue
		}
		if readErr != nil {
			return false, readErr
		}
		if writeErr := b.duffel.WriteFile(ctx, paths[1], content); writeErr != nil {
			return false, writeErr
		}
	}
	// memory.md is the bootstrap completion marker, so write it last. A partial
	// journal copy remains safely retryable if the final write fails.
	if err := b.duffel.WriteFile(ctx, FilePath(destination), memoryDoc); err != nil {
		return false, err
	}
	return true, nil
}

// pruneLocked keeps the done map bounded before recording a new entry. It must
// be called under b.mu. It first drops entries older than retryAfter (stale and
// re-derivable), then, if still at/over cap (and project is a new key), evicts
// the oldest entry by timestamp. The cap is small enough that the full scan is
// cheap and bounded.
func (b *Bootstrapper) pruneLocked(project string) {
	now := time.Now()
	for k, when := range b.done {
		if now.Sub(when) >= retryAfter {
			delete(b.done, k)
		}
	}
	if _, replacing := b.done[project]; replacing {
		return // re-recording an existing key doesn't grow the map
	}
	if len(b.done) < maxBootstrapEntries {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, when := range b.done {
		if oldestKey == "" || when.Before(oldest) {
			oldestKey, oldest = k, when
		}
	}
	if oldestKey != "" {
		delete(b.done, oldestKey)
	}
}

func (b *Bootstrapper) forget(project string) {
	b.mu.Lock()
	delete(b.done, project)
	b.mu.Unlock()
}

// memorySkeleton is the initial curated memory document. The compactor
// preserves its heading; the Repository line keeps the project↔git mapping
// visible to humans and agents browsing duffel.
func memorySkeleton(project, repoURL string) string {
	var sb strings.Builder
	sb.WriteString("# Project memory: " + project + "\n\n")
	if repoURL != "" {
		sb.WriteString("Repository: " + repoURL + "\n\n")
	}
	sb.WriteString("_Curated knowledge for this project. Maintained automatically: " +
		"agents record observations into lessons.md and the proxy's compactor " +
		"periodically folds them in here. Edit freely — human curation wins._\n")
	return sb.String()
}
