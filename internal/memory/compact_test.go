package memory

import (
	"context"
	"errors"
	"maps"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
)

type fakeCompactionQueue struct {
	projects []string
	err      error
}

func (f *fakeCompactionQueue) EnqueueProject(ctx context.Context, project string) error {
	f.projects = append(f.projects, project)
	return f.err
}

// recordingDuffel implements duffelFS and records mutation order.
type recordingDuffel struct {
	files   map[string]string
	entries []duffel.DirEntry
	ops     []string
}

func (r *recordingDuffel) ReadFile(ctx context.Context, path string) (string, error) {
	content, ok := r.files[path]
	if !ok {
		return "", duffel.ErrNotFound
	}
	return content, nil
}

func (r *recordingDuffel) WriteFile(ctx context.Context, path, content string) error {
	r.ops = append(r.ops, "write "+path)
	r.files[path] = content
	return nil
}

func (r *recordingDuffel) JournalAppend(ctx context.Context, path, content string) error {
	r.ops = append(r.ops, "append "+path)
	return nil
}

func (r *recordingDuffel) Archive(ctx context.Context, path string) error {
	r.ops = append(r.ops, "archive "+path)
	delete(r.files, path)
	return nil
}

func (r *recordingDuffel) ListDir(ctx context.Context, path string) ([]duffel.DirEntry, error) {
	return r.entries, nil
}

func TestCompactBelowThresholdNoOp(t *testing.T) {
	d := &recordingDuffel{files: map[string]string{
		"projects/demo/memory.md": "short",
	}}
	c := NewCompactor(d, newMockLLM(), CompactorConfig{Model: "m", ThresholdTokens: 8000}, zerolog.Nop())
	if err := c.CompactProject(context.Background(), "demo"); err != nil {
		t.Fatalf("CompactProject: %v", err)
	}
	if len(d.ops) != 0 {
		t.Errorf("ops = %v, want none", d.ops)
	}
}

func TestCompactHappyPathOrder(t *testing.T) {
	big := strings.Repeat("lesson text. ", 3000) // way over threshold
	d := &recordingDuffel{files: map[string]string{
		"projects/demo/memory.md":  "# old memory",
		"projects/demo/lessons.md": "---\ntype: journal\n---\n\n## 2026-06-01 10:00\n" + big,
	}}
	llm := newMockLLMText("# Project memory: demo\n\n## Pitfalls\n- merged lesson")
	c := NewCompactor(d, llm, CompactorConfig{Model: "m", ThresholdTokens: 1000}, zerolog.Nop())

	if err := c.CompactProject(context.Background(), "demo"); err != nil {
		t.Fatalf("CompactProject: %v", err)
	}

	want := []string{
		"write projects/demo/memory.md",
		"archive projects/demo/lessons.md",
		"append projects/demo/lessons.md",
	}
	if len(d.ops) != len(want) {
		t.Fatalf("ops = %v, want %v", d.ops, want)
	}
	for i := range want {
		if d.ops[i] != want[i] {
			t.Fatalf("ops = %v, want %v (write must precede archive)", d.ops, want)
		}
	}
	if !strings.Contains(d.files["projects/demo/memory.md"], "merged lesson") {
		t.Error("memory.md should hold compacted output")
	}
	req := llm.request(t, 0)
	if req.Model != "m" {
		t.Errorf("LLM model = %q", req.Model)
	}
	if !strings.Contains(req.System, "1200") {
		t.Errorf("LLM system prompt should include target token budget, got %q", req.System)
	}
	if req.MaxTokens != 1200 {
		t.Errorf("LLM max tokens = %d, want hard cap 1200", req.MaxTokens)
	}
	if !strings.Contains(req.User, "## Current curated memory") || !strings.Contains(req.User, "## Lessons journal") {
		t.Errorf("LLM user prompt missing compaction sections:\n%s", req.User)
	}
}

func TestCompactRejectsBadOutput(t *testing.T) {
	big := strings.Repeat("lesson text. ", 3000)
	files := map[string]string{
		"projects/demo/memory.md":  "# old memory",
		"projects/demo/lessons.md": big,
	}

	cases := map[string]string{
		"empty":         "",
		"wrong heading": "## Not the heading\ncontent",
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			d := &recordingDuffel{files: maps.Clone(files)}
			c := NewCompactor(d, newMockLLMText(response), CompactorConfig{Model: "m", ThresholdTokens: 1000}, zerolog.Nop())
			if err := c.CompactProject(context.Background(), "demo"); err == nil {
				t.Fatal("expected rejection")
			}
			if len(d.ops) != 0 {
				t.Errorf("rejected output must not mutate anything; ops = %v", d.ops)
			}
		})
	}
}

func TestCompactRejectsOutputOverHardCap(t *testing.T) {
	big := strings.Repeat("lesson text. ", 3000)
	heading := "# Project memory: demo\n"
	response := heading + strings.Repeat("x", 401)
	d := &recordingDuffel{files: map[string]string{
		"projects/demo/memory.md":  "# old memory",
		"projects/demo/lessons.md": big,
	}}
	c := NewCompactor(d, newMockLLMText(response), CompactorConfig{
		Model:           "m",
		ThresholdTokens: 1000,
		MaxTokens:       100,
	}, zerolog.Nop())

	err := c.CompactProject(context.Background(), "demo")
	if err == nil || !strings.Contains(err.Error(), "output exceeds hard cap") {
		t.Fatalf("CompactProject error = %v, want hard-cap rejection", err)
	}
	if len(d.ops) != 0 {
		t.Fatalf("rejected output must not mutate project files, ops = %v", d.ops)
	}
}

func TestCompactLLMErrorLeavesFilesUntouched(t *testing.T) {
	big := strings.Repeat("lesson text. ", 3000)
	d := &recordingDuffel{files: map[string]string{
		"projects/demo/lessons.md": big,
	}}
	c := NewCompactor(d, newMockLLMError(errors.New("boom")), CompactorConfig{Model: "m", ThresholdTokens: 1000}, zerolog.Nop())
	if err := c.CompactProject(context.Background(), "demo"); err == nil {
		t.Fatal("expected error")
	}
	if len(d.ops) != 0 {
		t.Errorf("ops = %v, want none", d.ops)
	}
}

// raceDuffel models the M7 mid-compaction race: the lessons journal grows
// between the initial snapshot read (CompactProject line ~109) and the re-read
// performed just before Archive. lessonsReads counts reads of lessons.md and
// swaps in the extended journal after the first read.
type raceDuffel struct {
	recordingDuffel
	lessonsPath     string
	snapshot        string
	extended        string
	lessonsReads    int
	appendedContent []string
}

func (r *raceDuffel) ReadFile(ctx context.Context, path string) (string, error) {
	if path == r.lessonsPath {
		r.lessonsReads++
		if r.lessonsReads == 1 {
			return r.snapshot, nil // snapshot fed to the LLM
		}
		return r.extended, nil // re-read sees a freshly appended entry
	}
	return r.recordingDuffel.ReadFile(ctx, path)
}

func (r *raceDuffel) JournalAppend(ctx context.Context, path, content string) error {
	r.appendedContent = append(r.appendedContent, content)
	return r.recordingDuffel.JournalAppend(ctx, path, content)
}

// TestCompactReplaysMidCompactionEntries verifies M7: a lesson appended while
// the compactor's LLM call is in flight is replayed into the recreated journal
// instead of being silently dropped by Archive.
func TestCompactReplaysMidCompactionEntries(t *testing.T) {
	big := strings.Repeat("lesson text. ", 3000) // over threshold
	snapshot := "---\ntype: journal\n---\n\n## 2026-06-01 10:00\n" + big
	// Same prefix plus one new entry appended mid-compaction.
	newEntry := "Discovered the staging DB needs a manual migrate-up."
	extended := snapshot + "\n---\n\n## 2026-06-01 10:05\n" + newEntry

	d := &raceDuffel{
		recordingDuffel: recordingDuffel{files: map[string]string{
			"projects/demo/memory.md":  "# old memory",
			"projects/demo/lessons.md": snapshot,
		}},
		lessonsPath: "projects/demo/lessons.md",
		snapshot:    snapshot,
		extended:    extended,
	}
	llm := newMockLLMText("# Project memory: demo\n\n## Pitfalls\n- merged lesson")
	c := NewCompactor(d, llm, CompactorConfig{Model: "m", ThresholdTokens: 1000}, zerolog.Nop())

	if err := c.CompactProject(context.Background(), "demo"); err != nil {
		t.Fatalf("CompactProject: %v", err)
	}

	// Op order: write, archive, append(marker), append(replayed entry).
	want := []string{
		"write projects/demo/memory.md",
		"archive projects/demo/lessons.md",
		"append projects/demo/lessons.md",
		"append projects/demo/lessons.md",
	}
	if len(d.ops) != len(want) {
		t.Fatalf("ops = %v, want %v", d.ops, want)
	}
	for i := range want {
		if d.ops[i] != want[i] {
			t.Fatalf("ops = %v, want %v", d.ops, want)
		}
	}
	// The mid-compaction entry must have been replayed verbatim.
	var replayed bool
	for _, content := range d.appendedContent {
		if strings.Contains(content, newEntry) {
			replayed = true
		}
	}
	if !replayed {
		t.Errorf("mid-compaction entry was lost; appended = %v", d.appendedContent)
	}
}

// TestCompactNoReplayWhenJournalUnchanged guards the M7 diff against
// mis-classifying the original (now-archived) entries as new: an unchanged
// journal must produce exactly the happy-path 3 ops (no spurious replays).
func TestCompactNoReplayWhenJournalUnchanged(t *testing.T) {
	big := strings.Repeat("lesson text. ", 3000)
	snapshot := "---\ntype: journal\n---\n\n## 2026-06-01 10:00\n" + big

	// raceDuffel returns the same content on both reads (extended == snapshot).
	d := &raceDuffel{
		recordingDuffel: recordingDuffel{files: map[string]string{
			"projects/demo/memory.md":  "# old memory",
			"projects/demo/lessons.md": snapshot,
		}},
		lessonsPath: "projects/demo/lessons.md",
		snapshot:    snapshot,
		extended:    snapshot,
	}
	llm := newMockLLMText("# Project memory: demo\n\n## Pitfalls\n- merged lesson")
	c := NewCompactor(d, llm, CompactorConfig{Model: "m", ThresholdTokens: 1000}, zerolog.Nop())

	if err := c.CompactProject(context.Background(), "demo"); err != nil {
		t.Fatalf("CompactProject: %v", err)
	}
	want := []string{
		"write projects/demo/memory.md",
		"archive projects/demo/lessons.md",
		"append projects/demo/lessons.md",
	}
	if len(d.ops) != len(want) {
		t.Fatalf("unchanged journal must yield 3 ops, got %v", d.ops)
	}
}

func TestSweepCompactsProjectDirs(t *testing.T) {
	big := strings.Repeat("x ", 10000)
	d := &recordingDuffel{
		files: map[string]string{
			"projects/demo/lessons.md": big,
		},
		entries: []duffel.DirEntry{
			{Name: "demo", IsDir: true},
			{Name: "readme.md", IsDir: false},
		},
	}
	llm := newMockLLMText("# Project memory: demo\ncompact")
	c := NewCompactor(d, llm, CompactorConfig{Model: "m", ThresholdTokens: 1000}, zerolog.Nop())
	c.Sweep(context.Background())
	if len(d.ops) == 0 {
		t.Error("sweep should compact the over-threshold project")
	}
}

func TestSweepQueuesOverThresholdProjectWhenQueueConfigured(t *testing.T) {
	big := strings.Repeat("x ", 10000)
	d := &recordingDuffel{
		files: map[string]string{
			"projects/demo/lessons.md": big,
		},
		entries: []duffel.DirEntry{
			{Name: "demo", IsDir: true},
			{Name: "small", IsDir: true},
		},
	}
	llm := newMockLLMText("# Project memory: demo\ncompact")
	c := NewCompactor(d, llm, CompactorConfig{Model: "m", ThresholdTokens: 1000}, zerolog.Nop())
	queue := &fakeCompactionQueue{}
	c.SetQueue(queue)

	c.Sweep(context.Background())

	if got, want := queue.projects, []string{"demo"}; !slicesEqual(got, want) {
		t.Fatalf("queued projects = %v, want %v", got, want)
	}
	if len(d.ops) != 0 {
		t.Fatalf("queued sweep must not compact inline; ops = %v", d.ops)
	}
	if len(llm.requests) != 0 {
		t.Fatalf("queued sweep must not call LLM inline; requests = %d", len(llm.requests))
	}
}

func TestSweepDoesNotFallBackToInlineCompactionWhenQueueFails(t *testing.T) {
	big := strings.Repeat("x ", 10000)
	d := &recordingDuffel{
		files: map[string]string{
			"projects/demo/lessons.md": big,
		},
		entries: []duffel.DirEntry{{Name: "demo", IsDir: true}},
	}
	llm := newMockLLMText("# Project memory: demo\ncompact")
	c := NewCompactor(d, llm, CompactorConfig{Model: "m", ThresholdTokens: 1000}, zerolog.Nop())
	c.SetQueue(&fakeCompactionQueue{err: errors.New("queue down")})

	c.Sweep(context.Background())

	if len(d.ops) != 0 {
		t.Fatalf("queue failure must not compact inline; ops = %v", d.ops)
	}
	if len(llm.requests) != 0 {
		t.Fatalf("queue failure must not call LLM inline; requests = %d", len(llm.requests))
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
