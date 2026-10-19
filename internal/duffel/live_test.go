package duffel

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLiveDuffel exercises the client against a real duffellib workspace.
// Skipped unless DUFFEL_LIVE_ROOT is set, e.g.:
//
//	DUFFEL_LIVE_ROOT=$(mktemp -d) go test -run TestLiveDuffel ./internal/duffel/
func TestLiveDuffel(t *testing.T) {
	root := os.Getenv("DUFFEL_LIVE_ROOT")
	if root == "" {
		t.Skip("DUFFEL_LIVE_ROOT not set")
	}

	c := New(Config{Root: root})
	ctx := context.Background()
	project := "live-smoke-" + strings.ToLower(time.Now().Format("150405"))
	base := "projects/" + project

	t.Run("read missing is ErrNotFound", func(t *testing.T) {
		if _, err := c.ReadFile(ctx, base+"/memory.md"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
	})

	t.Run("write and read memory.md", func(t *testing.T) {
		if err := c.WriteFile(ctx, base+"/memory.md", "# Project memory\n\nUse pnpm."); err != nil {
			t.Fatal(err)
		}
		content, err := c.ReadFile(ctx, base+"/memory.md")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(content, "Use pnpm.") {
			t.Errorf("content = %q", content)
		}
	})

	t.Run("journal append creates then appends", func(t *testing.T) {
		if err := c.JournalAppend(ctx, base+"/lessons.md", "**[lesson]** first"); err != nil {
			t.Fatal(err)
		}
		if err := c.JournalAppend(ctx, base+"/lessons.md", "**[mistake]** second"); err != nil {
			t.Fatal(err)
		}
		content, err := c.ReadFile(ctx, base+"/lessons.md")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(content, "type: journal") {
			t.Errorf("missing journal frontmatter:\n%s", content)
		}
		if !strings.Contains(content, "first") || !strings.Contains(content, "second") {
			t.Errorf("missing entries:\n%s", content)
		}
	})

	t.Run("concurrent appends to a fresh journal lose nothing", func(t *testing.T) {
		path := base + "/race.md"
		var wg sync.WaitGroup
		errs := make([]error, 4)
		for i := range errs {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				errs[n] = c.JournalAppend(ctx, path, "entry")
			}(i)
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}
		content, err := c.ReadFile(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(content, "entry"); got != len(errs) {
			t.Errorf("journal has %d entries, want %d:\n%s", got, len(errs), content)
		}
	})

	t.Run("list projects dir", func(t *testing.T) {
		entries, err := c.ListDir(ctx, "projects")
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range entries {
			if e.Name == project && e.IsDir {
				found = true
			}
		}
		if !found {
			t.Errorf("project %s not in listing: %+v", project, entries)
		}
	})

	t.Run("archive then recreate journal", func(t *testing.T) {
		if err := c.Archive(ctx, base+"/lessons.md"); err != nil {
			t.Fatal(err)
		}
		if _, err := c.ReadFile(ctx, base+"/lessons.md"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("archived journal still readable: %v", err)
		}
		if err := c.JournalAppend(ctx, base+"/lessons.md", "post-compaction marker"); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("search tolerated", func(t *testing.T) {
		// qmd may not be indexed in a fresh data dir; both outcomes are OK,
		// only a hang or panic would be a bug.
		results, err := c.Search(ctx, "pnpm", SearchOpts{Limit: 3})
		t.Logf("search: %d results, err=%v", len(results), err)
	})
}
