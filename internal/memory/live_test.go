package memory

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
)

// TestLiveBundle assembles a real context bundle against a live duffellib
// workspace. Skipped unless DUFFEL_LIVE_ROOT is set.
func TestLiveBundle(t *testing.T) {
	root := os.Getenv("DUFFEL_LIVE_ROOT")
	if root == "" {
		t.Skip("DUFFEL_LIVE_ROOT not set")
	}

	client := duffel.New(duffel.Config{Root: root})
	ctx := context.Background()
	project := "live-bundle-" + strings.ToLower(time.Now().Format("150405"))

	// Seed: curated memory + a few journal lessons.
	if err := client.WriteFile(ctx, FilePath(project), "# Facts\n\n- Use pnpm, not npm.\n- Migrations are manual (make migrate-up)."); err != nil {
		t.Fatal(err)
	}
	for _, lesson := range []string{
		"**[mistake]** (source: agent)\n\nAssumed quota was per-user; it is per-key.",
		"**[lesson]** (source: distiller, conversation: x)\n\nRun gofmt before committing; CI rejects unformatted files.",
	} {
		if err := client.JournalAppend(ctx, LessonsFilePath(project), lesson); err != nil {
			t.Fatal(err)
		}
	}

	builder := NewBuilder(client, BundleConfig{}, nil, zerolog.Nop())
	bundle := builder.Build(ctx, project, "quota and migrations", 0, true)

	if bundle.Degraded {
		t.Fatal("bundle degraded against live duffel")
	}
	for _, want := range []string{"# Project memory: " + project, "Use pnpm", "per-key", "gofmt", "record_memory"} {
		if !strings.Contains(bundle.Markdown, want) {
			t.Errorf("bundle missing %q", want)
		}
	}
	t.Logf("live bundle (%d tokens):\n%s", bundle.TokenEstimate, bundle.Markdown)
}
