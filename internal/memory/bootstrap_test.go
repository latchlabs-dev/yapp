package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
)

type fakeBootstrapDuffel struct {
	files   map[string]string
	readErr error
	writes  []string
	appends []string
}

func (f *fakeBootstrapDuffel) ReadFile(ctx context.Context, path string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	content, ok := f.files[path]
	if !ok {
		return "", duffel.ErrNotFound
	}
	return content, nil
}

func (f *fakeBootstrapDuffel) WriteFile(ctx context.Context, path, content string) error {
	f.writes = append(f.writes, path)
	f.files[path] = content
	return nil
}

func (f *fakeBootstrapDuffel) JournalAppend(ctx context.Context, path, content string) error {
	f.appends = append(f.appends, path)
	return nil
}

func TestBootstrapCreatesStructure(t *testing.T) {
	d := &fakeBootstrapDuffel{files: map[string]string{}}
	b := NewBootstrapper(d, zerolog.Nop())

	b.Ensure(context.Background(), "newproj", "git@github.com:org/newproj.git")

	if len(d.writes) != 1 || d.writes[0] != "projects/newproj/memory.md" {
		t.Errorf("writes = %v", d.writes)
	}
	if len(d.appends) != 1 || d.appends[0] != "projects/newproj/lessons.md" {
		t.Errorf("appends = %v", d.appends)
	}
	skeleton := d.files["projects/newproj/memory.md"]
	if !strings.Contains(skeleton, "# Project memory: newproj") {
		t.Errorf("skeleton missing heading:\n%s", skeleton)
	}
	if !strings.Contains(skeleton, "git@github.com:org/newproj.git") {
		t.Errorf("skeleton missing repo URL:\n%s", skeleton)
	}
}

func TestBootstrapIdempotent(t *testing.T) {
	d := &fakeBootstrapDuffel{files: map[string]string{}}
	b := NewBootstrapper(d, zerolog.Nop())

	b.Ensure(context.Background(), "proj", "")
	b.Ensure(context.Background(), "proj", "")
	if len(d.writes) != 1 {
		t.Errorf("writes = %v, want exactly one", d.writes)
	}
}

func TestBootstrapSkipsExistingProject(t *testing.T) {
	d := &fakeBootstrapDuffel{files: map[string]string{
		"projects/existing/memory.md": "# already here",
	}}
	b := NewBootstrapper(d, zerolog.Nop())

	b.Ensure(context.Background(), "existing", "")
	if len(d.writes) != 0 || len(d.appends) != 0 {
		t.Errorf("existing project must not be touched: writes=%v appends=%v", d.writes, d.appends)
	}
}

func TestBootstrapMigratesOnlyExplicitTenantScopedLegacyProject(t *testing.T) {
	d := &fakeBootstrapDuffel{files: map[string]string{
		"projects/tenant-1--demo/memory.md":             "# Existing memory\n",
		"projects/tenant-1--demo/lessons.md":            "existing lessons\n",
		"projects/tenant-1--demo/prompt-reflections.md": "existing reflections\n",
		"projects/demo/memory.md":                       "# Unsafe unscoped memory\n",
	}}
	b := NewBootstrapper(d, zerolog.Nop())
	b.EnsureCanonical(context.Background(), "tenant-1--mprj_shared", "tenant-1--demo", "https://example.com/demo.git")

	if got := d.files["projects/tenant-1--mprj_shared/memory.md"]; got != "# Existing memory\n" {
		t.Fatalf("memory = %q", got)
	}
	if got := d.files["projects/tenant-1--mprj_shared/lessons.md"]; got != "existing lessons\n" {
		t.Fatalf("lessons = %q", got)
	}
	if got := d.files["projects/tenant-1--mprj_shared/prompt-reflections.md"]; got != "existing reflections\n" {
		t.Fatalf("reflections = %q", got)
	}
}

func TestBootstrapRetriesAfterProbeFailure(t *testing.T) {
	d := &fakeBootstrapDuffel{files: map[string]string{}, readErr: errors.New("duffel down")}
	b := NewBootstrapper(d, zerolog.Nop())

	b.Ensure(context.Background(), "proj", "")
	if len(d.writes) != 0 {
		t.Fatal("must not write while duffel is unhealthy")
	}

	// Duffel recovers; the failed attempt must not be remembered as done.
	d.readErr = nil
	b.forget("proj") // simulate retryAfter elapsing
	b.Ensure(context.Background(), "proj", "")
	if len(d.writes) != 1 {
		t.Errorf("writes = %v, want bootstrap after recovery", d.writes)
	}
}

// TestBootstrapDoneCapBounded verifies M5: the done map stays bounded across
// many distinct project names rather than growing one entry per project forever.
func TestBootstrapDoneCapBounded(t *testing.T) {
	d := &fakeBootstrapDuffel{files: map[string]string{}}
	b := NewBootstrapper(d, zerolog.Nop())
	for i := 0; i < maxBootstrapEntries+50; i++ {
		b.Ensure(context.Background(), fmt.Sprintf("proj%d", i), "")
	}
	b.mu.Lock()
	size := len(b.done)
	b.mu.Unlock()
	if size > maxBootstrapEntries {
		t.Fatalf("done map size = %d, want <= %d", size, maxBootstrapEntries)
	}
}

// TestBootstrapPrunesStaleEntries verifies stale (> retryAfter) entries are
// pruned on the next Ensure so the map doesn't accumulate dead entries.
func TestBootstrapPrunesStaleEntries(t *testing.T) {
	d := &fakeBootstrapDuffel{files: map[string]string{
		"projects/live/memory.md": "exists",
	}}
	b := NewBootstrapper(d, zerolog.Nop())

	// Seed a stale entry directly (older than retryAfter).
	b.mu.Lock()
	b.done["stale"] = time.Now().Add(-2 * retryAfter)
	b.mu.Unlock()

	// Any Ensure call runs pruneLocked, which must drop the stale entry.
	b.Ensure(context.Background(), "live", "")

	b.mu.Lock()
	_, stalePresent := b.done["stale"]
	b.mu.Unlock()
	if stalePresent {
		t.Error("stale entry should have been pruned")
	}
}

func TestBundleNoiseFloor(t *testing.T) {
	// A bootstrapped-but-unused project: skeleton only.
	f := &fakeDuffel{files: map[string]string{
		"projects/fresh/memory.md": memorySkeleton("fresh", "git@github.com:org/fresh.git"),
	}}
	b := NewBuilder(f, BundleConfig{MinContentTokens: 80}, nil, zerolog.Nop())

	bundle := b.Build(context.Background(), "fresh", "", 0, false)
	if bundle.Markdown != "" {
		t.Errorf("skeleton-only bundle should be suppressed, got %d tokens:\n%s", bundle.TokenEstimate, bundle.Markdown)
	}

	// Once real lessons accumulate, the bundle flows.
	f2 := &fakeDuffel{files: map[string]string{
		"projects/fresh/memory.md":  memorySkeleton("fresh", ""),
		"projects/fresh/lessons.md": sampleJournal,
	}}
	b2 := NewBuilder(f2, BundleConfig{MinContentTokens: 80}, nil, zerolog.Nop())
	bundle2 := b2.Build(context.Background(), "fresh", "", 0, false)
	if bundle2.Markdown == "" {
		t.Error("bundle with real lessons should not be suppressed")
	}
}
