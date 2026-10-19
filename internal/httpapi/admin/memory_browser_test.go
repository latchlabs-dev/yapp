package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
)

func TestArchiveMemoryProject(t *testing.T) {
	client := duffel.New(duffel.Config{Root: t.TempDir()})
	if err := client.WriteFile(context.Background(), "projects/demo/memory.md", "# Demo"); err != nil {
		t.Fatal(err)
	}
	router := NewRouter(&Dependencies{
		AdminKey:     "test-admin-key",
		DuffelClient: client,
		Logger:       zerolog.Nop(),
	})

	req := httptest.NewRequest(http.MethodPost, "/memory/projects/demo/archive", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var archived struct {
		Status       string `json:"status"`
		ArchivedPath string `json:"archived_path"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&archived); err != nil {
		t.Fatal(err)
	}
	if archived.Status != "archived" || !strings.HasPrefix(archived.ArchivedPath, "projects/.archive/demo-") {
		t.Fatalf("archive response = %+v", archived)
	}

	req = httptest.NewRequest(http.MethodGet, "/memory/projects", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var listed struct {
		Projects []projectSummary `json:"projects"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Projects) != 0 {
		t.Fatalf("active projects = %+v, want none", listed.Projects)
	}

	content, err := client.ReadFile(context.Background(), archived.ArchivedPath+"/memory.md")
	if err != nil || content != "# Demo" {
		t.Fatalf("archived memory = %q, err = %v", content, err)
	}
}

func TestArchiveMemoryProjectNotFound(t *testing.T) {
	client := duffel.New(duffel.Config{Root: t.TempDir()})
	router := NewRouter(&Dependencies{AdminKey: "test-admin-key", DuffelClient: client})
	req := httptest.NewRequest(http.MethodPost, "/memory/projects/missing/archive", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
