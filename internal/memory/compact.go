package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/llmclient"
)

// CompactorConfig tunes the periodic memory compaction job.
type CompactorConfig struct {
	Model           string        // LLM used for compaction (required)
	Interval        time.Duration // how often to scan projects (default 6h)
	ThresholdTokens int           // compact when memory+lessons exceed this (default 8000)
	MaxTokens       int           // hard cap for generated curated memory (default 1200)
}

const DefaultCompactionMaxTokens = 1200

func (c CompactorConfig) withDefaults() CompactorConfig {
	if c.Interval <= 0 {
		c.Interval = 6 * time.Hour
	}
	if c.ThresholdTokens <= 0 {
		c.ThresholdTokens = 8000
	}
	if c.MaxTokens <= 0 {
		c.MaxTokens = DefaultCompactionMaxTokens
	}
	return c
}

// duffelFS is the duffel client subset the compactor needs.
type duffelFS interface {
	ReadFile(ctx context.Context, path string) (string, error)
	WriteFile(ctx context.Context, path, content string) error
	JournalAppend(ctx context.Context, path, content string) error
	Archive(ctx context.Context, path string) error
	ListDir(ctx context.Context, path string) ([]duffel.DirEntry, error)
}

type compactionEnqueuer interface {
	EnqueueProject(ctx context.Context, project string) error
}

// Compactor periodically folds each project's lessons journal into its
// curated memory document, keeping injected context small and deduplicated.
type Compactor struct {
	duffel duffelFS
	llm    completer
	cfg    CompactorConfig
	queue  compactionEnqueuer
	logger zerolog.Logger
}

// NewCompactor creates a compactor.
func NewCompactor(d duffelFS, llm completer, cfg CompactorConfig, logger zerolog.Logger) *Compactor {
	return &Compactor{duffel: d, llm: llm, cfg: cfg.withDefaults(), logger: logger}
}

func (c *Compactor) SetQueue(queue compactionEnqueuer) {
	c.queue = queue
}

// Start runs the compaction loop until ctx is cancelled.
func (c *Compactor) Start(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()

	c.logger.Info().
		Str("model", c.cfg.Model).
		Dur("interval", c.cfg.Interval).
		Int("threshold_tokens", c.cfg.ThresholdTokens).
		Msg("memory compactor started")

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Sweep(ctx)
		}
	}
}

// Sweep checks every project directory and compacts those over threshold.
func (c *Compactor) Sweep(ctx context.Context) {
	entries, err := c.duffel.ListDir(ctx, "projects")
	if err != nil {
		if !isNotFound(err) {
			c.logger.Warn().Err(err).Msg("compactor: list projects failed")
		}
		return
	}

	for _, entry := range entries {
		if !entry.IsDir {
			continue
		}
		project, err := SanitizeProject(entry.Name)
		if err != nil {
			continue
		}
		totalTokens, needed, err := c.NeedsCompaction(ctx, project)
		if err != nil {
			c.logger.Warn().Err(err).Str("project", project).Msg("compaction threshold check failed")
			continue
		}
		if !needed {
			continue
		}
		if c.queue != nil {
			if err := c.queue.EnqueueProject(ctx, project); err != nil {
				c.logger.Warn().Err(err).Str("project", project).Int("tokens", totalTokens).Msg("compaction enqueue failed")
			}
			continue
		}
		if err := c.CompactProject(ctx, project); err != nil {
			c.logger.Warn().Err(err).Str("project", project).Int("tokens", totalTokens).Msg("compaction failed")
		}
	}
}

func (c *Compactor) NeedsCompaction(ctx context.Context, project string) (int, bool, error) {
	project, err := SanitizeProject(project)
	if err != nil {
		return 0, false, err
	}
	memoryDoc, lessonsDoc, err := c.readProjectDocs(ctx, project)
	if err != nil {
		return 0, false, err
	}
	totalTokens := EstimateTokens(memoryDoc) + EstimateTokens(lessonsDoc)
	return totalTokens, totalTokens > c.cfg.ThresholdTokens, nil
}

// CompactProject compacts one project if its memory exceeds the threshold.
// Destructive steps (archiving the journal) happen only after the new curated
// memory has been validated and written.
func (c *Compactor) CompactProject(ctx context.Context, project string) error {
	unlock := LockProject(project)
	defer unlock()

	memoryDoc, lessonsDoc, err := c.readProjectDocs(ctx, project)
	if err != nil {
		return err
	}

	totalTokens := EstimateTokens(memoryDoc) + EstimateTokens(lessonsDoc)
	if totalTokens <= c.cfg.ThresholdTokens {
		return nil
	}

	heading := "# Project memory: " + project
	targetTokens := c.cfg.MaxTokens
	system := fmt.Sprintf(compactorSystemPrompt, targetTokens, targetTokens*4)
	user := "Heading line: " + heading + "\n\n" +
		"## Current curated memory\n\n" + strings.TrimSpace(stripFrontmatter(memoryDoc)) + "\n\n" +
		"## Lessons journal\n\n" + strings.TrimSpace(stripFrontmatter(lessonsDoc))

	compacted, err := c.llm.Complete(ctx, llmclient.Request{
		Model:     c.cfg.Model,
		System:    system,
		User:      user,
		MaxTokens: targetTokens,
	})
	if err != nil {
		return fmt.Errorf("compactor LLM call: %w", err)
	}

	compacted = strings.TrimSpace(compacted)
	if err := validateCompacted(compacted, heading, totalTokens, c.cfg.MaxTokens); err != nil {
		return fmt.Errorf("compactor output rejected: %w", err)
	}

	// The LLM call above takes seconds; agents may append fresh lessons to the
	// journal during that window. Those entries were not part of the compaction
	// input, yet Archive would discard them. Duffel exposes no atomic swap
	// (Archive + JournalAppend are separate calls), so just before the
	// destructive steps we re-read the journal and capture any entries appended
	// since our snapshot, then replay them into the recreated journal so they
	// survive and are folded in on the next compaction.
	replay := c.newLessonsSince(ctx, project, lessonsDoc)

	// Write the new curated memory first; only then archive the journal.
	if err := c.duffel.WriteFile(ctx, FilePath(project), compacted+"\n"); err != nil {
		return fmt.Errorf("write compacted memory: %w", err)
	}
	if lessonsDoc != "" {
		if err := c.duffel.Archive(ctx, LessonsFilePath(project)); err != nil {
			return fmt.Errorf("archive lessons journal: %w", err)
		}
		marker := "Compacted into memory.md (previous journal archived under .archive/)."
		if err := c.duffel.JournalAppend(ctx, LessonsFilePath(project), marker); err != nil {
			return fmt.Errorf("recreate lessons journal: %w", err)
		}
		for _, content := range replay {
			if err := c.duffel.JournalAppend(ctx, LessonsFilePath(project), content); err != nil {
				return fmt.Errorf("replay mid-compaction lesson: %w", err)
			}
		}
	}

	c.logger.Info().
		Str("project", project).
		Int("before_tokens", totalTokens).
		Int("after_tokens", EstimateTokens(compacted)).
		Msg("project memory compacted")
	return nil
}

func (c *Compactor) readProjectDocs(ctx context.Context, project string) (memoryDoc, lessonsDoc string, err error) {
	memoryDoc, err = c.duffel.ReadFile(ctx, FilePath(project))
	if err != nil && !isNotFound(err) {
		return "", "", fmt.Errorf("read memory: %w", err)
	}
	lessonsDoc, err = c.duffel.ReadFile(ctx, LessonsFilePath(project))
	if err != nil && !isNotFound(err) {
		return "", "", fmt.Errorf("read lessons: %w", err)
	}
	return memoryDoc, lessonsDoc, nil
}

// newLessonsSince re-reads the lessons journal and returns the content of any
// entries appended after the snapshot was captured. The journal is append-only,
// so in the normal case the snapshot's entries are a prefix of the fresh read
// and the new entries are exactly the tail beyond len(snapshot). If the fresh
// read fails, is missing, or no longer has the snapshot as a prefix (e.g. it was
// concurrently archived/rotated), we conservatively replay nothing rather than
// risk duplicating already-folded entries.
func (c *Compactor) newLessonsSince(ctx context.Context, project, snapshot string) []string {
	fresh, err := c.duffel.ReadFile(ctx, LessonsFilePath(project))
	if err != nil {
		if !isNotFound(err) {
			c.logger.Warn().Err(err).Str("project", project).Msg("compactor: re-read lessons for replay failed; skipping replay")
		}
		return nil
	}

	snapEntries := ParseJournalEntries(snapshot)
	freshEntries := ParseJournalEntries(fresh)
	if len(freshEntries) <= len(snapEntries) {
		return nil // nothing new appended (or the journal shrank/rotated)
	}
	// Verify the snapshot is still a prefix; if not, the journal was rotated
	// under us and the tail offset is meaningless — skip replay.
	for i := range snapEntries {
		if freshEntries[i].Content != snapEntries[i].Content {
			return nil
		}
	}

	replay := make([]string, 0, len(freshEntries)-len(snapEntries))
	for _, e := range freshEntries[len(snapEntries):] {
		replay = append(replay, e.Content)
	}
	return replay
}

// validateCompacted sanity-checks LLM output before anything destructive.
func validateCompacted(compacted, heading string, inputTokens, maxTokens int) error {
	if compacted == "" {
		return fmt.Errorf("empty output")
	}
	if !strings.HasPrefix(compacted, heading) {
		return fmt.Errorf("output does not start with expected heading %q", heading)
	}
	outputTokens := EstimateTokens(compacted)
	if outputTokens > maxTokens {
		return fmt.Errorf("output exceeds hard cap (%d > %d tokens)", outputTokens, maxTokens)
	}
	if outputTokens > inputTokens*2 {
		return fmt.Errorf("output larger than 2x input (%d tokens)", outputTokens)
	}
	return nil
}

func isNotFound(err error) bool {
	return errors.Is(err, duffel.ErrNotFound)
}
