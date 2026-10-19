package duffel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	c := New(Config{Root: t.TempDir()})
	t.Cleanup(func() { _ = c.workspace.Close() })
	return c
}

func TestReadWriteFile(t *testing.T) {
	c := newTestClient(t)

	if err := c.WriteFile(context.Background(), "projects/demo/memory.md", "# Memory"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	content, err := c.ReadFile(context.Background(), "projects/demo/memory.md")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if content != "# Memory" {
		t.Errorf("content = %q", content)
	}
}

func TestReadFileNotFound(t *testing.T) {
	c := newTestClient(t)
	_, err := c.ReadFile(context.Background(), "missing.md")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestJournalAppendCreatesThenAppends(t *testing.T) {
	c := newTestClient(t)

	if err := c.JournalAppend(context.Background(), "projects/demo/lessons.md", "first"); err != nil {
		t.Fatalf("JournalAppend first: %v", err)
	}
	if err := c.JournalAppend(context.Background(), "projects/demo/lessons.md", "second"); err != nil {
		t.Fatalf("JournalAppend second: %v", err)
	}

	content, err := c.ReadFile(context.Background(), "projects/demo/lessons.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"type: journal", "first", "second"} {
		if !strings.Contains(content, want) {
			t.Errorf("journal missing %q:\n%s", want, content)
		}
	}
}

func TestArchive(t *testing.T) {
	c := newTestClient(t)
	if err := c.JournalAppend(context.Background(), "projects/demo/lessons.md", "a lesson"); err != nil {
		t.Fatal(err)
	}
	if err := c.Archive(context.Background(), "projects/demo/lessons.md"); err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if _, err := c.ReadFile(context.Background(), "projects/demo/lessons.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("archived file err = %v, want ErrNotFound", err)
	}
}

func TestArchiveDir(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	if err := c.WriteFile(ctx, "projects/demo/memory.md", "# Memory"); err != nil {
		t.Fatal(err)
	}

	archivedPath, err := c.ArchiveDir(ctx, "projects/demo")
	if err != nil {
		t.Fatalf("ArchiveDir: %v", err)
	}
	if !strings.HasPrefix(archivedPath, "projects/.archive/demo-") {
		t.Fatalf("archived path = %q", archivedPath)
	}
	if _, err := c.ReadFile(ctx, "projects/demo/memory.md"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("source read err = %v, want ErrNotFound", err)
	}
	if content, err := c.ReadFile(ctx, archivedPath+"/memory.md"); err != nil || content != "# Memory" {
		t.Fatalf("archived content = %q, err = %v", content, err)
	}
}

func TestListDir(t *testing.T) {
	c := newTestClient(t)
	if err := c.WriteFile(context.Background(), "projects/demo/memory.md", "# Memory"); err != nil {
		t.Fatal(err)
	}
	entries, err := c.ListDir(context.Background(), "projects")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "demo" || !entries[0].IsDir {
		t.Errorf("entries = %v", entries)
	}
}

func TestSearchFallsBackWhenQmdIndexMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	c := newTestClient(t)
	if err := c.WriteFile(context.Background(), "projects/demo/memory.md", "# Demo\n\nUse the permission repair runbook for Duffel mount errors."); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteFile(context.Background(), "projects/other/memory.md", "# Other\n\nUnrelated note."); err != nil {
		t.Fatal(err)
	}

	results, err := c.Search(context.Background(), "permission repair", SearchOpts{Limit: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected fallback search result")
	}
	if results[0].Path != "projects/demo/memory.md" {
		t.Fatalf("top path = %q, want demo memory", results[0].Path)
	}
	if !strings.Contains(results[0].Snippet, "permission repair") {
		t.Fatalf("snippet = %q", results[0].Snippet)
	}
}

func TestTenantPathMatchesRejectsAmbiguousOwnership(t *testing.T) {
	for _, docPath := range []string{
		"projects/tenant-b--demo/memory.md",
		"projects/tenant-a2--demo/memory.md",
		"projects/TENANT-A--demo/memory.md",
		"projects/tenant-a--DEMO/memory.md",
		"projects/tenant-a---demo/memory.md",
		"projects/tenant-a--demo--extra/memory.md",
		"projects/demo/memory.md",
		"notes/tenant-a.md",
		"purgatory/tenant-a/user-user-1/observations.md",
		"/projects/tenant-a--demo/memory.md",
		"projects/tenant-a--demo/../foreign.md",
		"projects/tenant-a--demo/./memory.md",
		"projects/tenant-a--demo/.archive/old.md",
		"projects/tenant-a--demo/nested/.hidden.md",
		`projects/tenant-a--demo/..\foreign.md`,
	} {
		if TenantPathMatches(docPath, "tenant-a") {
			t.Errorf("ambiguous path matched tenant: %q", docPath)
		}
	}
	if !TenantPathMatches("projects/tenant-a--related/nested/notes.md", "tenant-a") {
		t.Fatal("legitimate same-tenant related note rejected")
	}
}

func TestSearchRejectsNoncanonicalTenantScope(t *testing.T) {
	c := newTestClient(t)
	for _, tenant := range []string{"TENANT-A", "../tenant-a", "tenant--a", ".tenant-a", "tenant-a."} {
		if _, err := c.Search(context.Background(), "context", SearchOpts{TenantScope: tenant}); err == nil {
			t.Errorf("noncanonical tenant scope accepted: %q", tenant)
		}
	}
}

// Set up the real qmd adapter and its metadata SQLite index, with a controlled
// executable standing in for the ranking engine. This proves filtering after
// index/URI/path conversion rather than testing only a fabricated helper result.
func stubQmdResults(t *testing.T, rows []map[string]any) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	indexDir := filepath.Join(home, ".cache", "qmd")
	if err := os.MkdirAll(indexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(indexDir, "index.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE documents (path TEXT, collection TEXT, modified_at TEXT, active INTEGER)`); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "qmd")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ncat <<'QMD_RESULTS'\n"+string(encoded)+"\nQMD_RESULTS\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DUFFEL_QMD_PATH", binary)
}

func TestSearchQmdIndexEnforcesTenantAndWorkspaceBoundaries(t *testing.T) {
	c := newTestClient(t)
	ownPath := "projects/tenant-a--related/note.md"
	if err := c.WriteFile(context.Background(), ownPath, "# Own note\n\nSame-tenant knowledge."); err != nil {
		t.Fatal(err)
	}
	stubQmdResults(t, []map[string]any{
		{"file": "qmd://duffel/projects/tenant-b--other/memory.md", "snippet": "FOREIGN tenant secret", "score": 1.0},
		{"file": "qmd://foreign/" + ownPath, "snippet": "FOREIGN collection secret", "score": 0.99},
		// A sibling root-prefix collision must not turn an absolute foreign
		// file into the same relative path as the existing own-tenant note.
		{"file": c.workspace.Root() + ownPath, "snippet": "FOREIGN workspace secret", "score": 0.95},
		{"file": "qmd://duffel/projects/tenant-a--deleted/memory.md", "snippet": "STALE deleted note", "score": 0.9},
		{"file": "qmd://duffel/" + ownPath, "title": "Own note", "snippet": "SAFE semantic result", "score": 0.8},
	})
	results, err := c.Search(context.Background(), "authorization boundary", SearchOpts{Limit: 1, TenantScope: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != ownPath || results[0].Snippet != "SAFE semantic result" {
		t.Fatalf("qmd result escaped the tenant/workspace boundary: %+v", results)
	}
}

func TestSearchForeignIndexHitsDoNotDisplaceTenantLexicalResults(t *testing.T) {
	c := newTestClient(t)
	ownPath := "projects/tenant-a--related/note.md"
	if err := c.WriteFile(context.Background(), ownPath, "# Own note\n\nAuthorization boundary SAFE lexical note."); err != nil {
		t.Fatal(err)
	}
	stubQmdResults(t, []map[string]any{
		{"file": "qmd://duffel/projects/tenant-b--other/memory.md", "snippet": "FOREIGN authorization boundary", "score": 1.0},
	})
	results, err := c.Search(context.Background(), "authorization boundary", SearchOpts{Limit: 1, TenantScope: "tenant-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Path != ownPath || !strings.Contains(results[0].Snippet, "SAFE lexical") {
		t.Fatalf("foreign indexed matches displaced an authorized note: %+v", results)
	}
}

func TestUnavailableWhenRootMissing(t *testing.T) {
	c := New(Config{})
	err := c.WriteFile(context.Background(), "a.md", "x")
	if err == nil {
		t.Fatal("expected error")
	}
}
