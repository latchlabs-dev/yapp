package observations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/memory"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type testProjectLookup struct{}

func (testProjectLookup) GetForTenant(_ context.Context, tenantID, projectID string) (*pgstore.MemoryProject, error) {
	if tenantID != "tenant-1" || projectID != "project-demo" {
		return nil, errors.New("project not found for tenant")
	}
	return &pgstore.MemoryProject{ID: projectID, TenantID: tenantID, Slug: "canonical-demo"}, nil
}

func newTestHandler(t *testing.T, broken bool) (http.HandlerFunc, *duffel.Client) {
	t.Helper()
	root := t.TempDir()
	if broken {
		root = ""
	}
	client := duffel.New(duffel.Config{Root: root})
	return NewHandler(client, testProjectLookup{}, 4096, zerolog.Nop()), client
}

func post(handler http.HandlerFunc, body string) *httptest.ResponseRecorder {
	return postAs(handler, body, &auth.Context{TenantID: "tenant-1", UserID: "user-1", KeyID: "key-1"})
}

func postAs(handler http.HandlerFunc, body string, caller *auth.Context) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/observations", strings.NewReader(body))
	if caller != nil {
		req = req.WithContext(middleware.WithAuthContext(req.Context(), caller))
	}
	rec := httptest.NewRecorder()
	middleware.RequestID(handler).ServeHTTP(rec, req)
	return rec
}

func TestObservationHappyPath(t *testing.T) {
	handler, client := newTestHandler(t, false)
	rec := post(handler, `{"project":"demo","project_id":"project-demo","kind":"mistake","content":"Assumed X, broke Y."}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	got, err := client.ReadFile(t.Context(), memory.LessonsFilePath("tenant-1--project-demo"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "type: journal") {
		t.Errorf("journal frontmatter missing: %s", got)
	}
	if !strings.Contains(got, "**[mistake]** (source: agent, user: user-1)") {
		t.Errorf("entry format wrong: %s", got)
	}
}

func TestObservationValidation(t *testing.T) {
	handler, _ := newTestHandler(t, false)
	cases := map[string]string{
		"bad kind":        `{"project":"demo","kind":"opinion","content":"x"}`,
		"empty content":   `{"project":"demo","kind":"lesson","content":"  "}`,
		"invalid project": `{"project":"..","kind":"lesson","content":"x"}`,
		"garbage":         `not json`,
	}
	for name, body := range cases {
		if rec := post(handler, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", name, rec.Code)
		}
	}
}

func TestObservationContentTooLarge(t *testing.T) {
	handler, _ := newTestHandler(t, false)
	rec := post(handler, `{"project":"demo","kind":"lesson","content":"`+strings.Repeat("a", 5000)+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestObservationDuffelDown(t *testing.T) {
	handler, _ := newTestHandler(t, true)
	for _, projectID := range []string{"", "project-demo"} {
		t.Run("project_id="+projectID, func(t *testing.T) {
			rec := post(handler, fmt.Sprintf(`{"project":"demo","project_id":%q,"kind":"lesson","content":"x"}`, projectID))
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", rec.Code)
			}
		})
	}
}

func TestObservationMissingProjectSavedInPurgatory(t *testing.T) {
	for _, withLookup := range []bool{true, false} {
		t.Run(fmt.Sprintf("project_lookup=%t", withLookup), func(t *testing.T) {
			root := t.TempDir()
			client := duffel.New(duffel.Config{Root: root})
			var lookup projectLookup
			if withLookup {
				lookup = testProjectLookup{}
			}
			handler := NewHandler(client, lookup, 4096, zerolog.Nop())
			rec := post(handler, `{"project":"Demo (checkout)","kind":"lesson","content":"Preserve this unresolved lesson."}`)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
			}
			var response map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response["status"] != "purgatory" || response["reason"] != "unresolved repository identity" {
				t.Fatalf("response = %v", response)
			}
			filePath, err := memory.PurgatoryFilePath("tenant-1", "user-1", "key-1")
			if err != nil {
				t.Fatal(err)
			}
			// A new workspace client must still see the accepted observation.
			reopened := duffel.New(duffel.Config{Root: root})
			got, err := reopened.ReadFile(t.Context(), filePath)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{
				"type: journal", "**[lesson]** (source: agent, status: purgatory)",
				`"project":"Demo (checkout)"`, `"tenant_id":"tenant-1"`,
				`"user_id":"user-1"`, `"key_id":"key-1"`, "Preserve this unresolved lesson.",
				`"request_id":"` + rec.Header().Get("X-Request-ID") + `"`,
			} {
				if !strings.Contains(got, want) {
					t.Errorf("purgatory journal missing %q: %s", want, got)
				}
			}
			if _, err := client.ListDir(t.Context(), "projects"); !errors.Is(err, duffel.ErrNotFound) {
				t.Fatalf("unresolved write created shared project memory: %v", err)
			}
		})
	}
}

func TestObservationPurgatoryOwnerIsolation(t *testing.T) {
	handler, client := newTestHandler(t, false)
	callers := []*auth.Context{
		{TenantID: "tenant-1", UserID: "user-1", KeyID: "key-1"},
		{TenantID: "tenant-2", UserID: "user-1", KeyID: "key-1"},
		{TenantID: "tenant-1", UserID: "user-2", KeyID: "key-1"},
		{TenantID: "tenant-1", KeyID: "key-1"},
		{TenantID: "tenant-1", KeyID: "key-2"},
	}
	for i, caller := range callers {
		rec := postAs(handler, fmt.Sprintf(`{"project":"demo","kind":"context","content":"private-note-%d"}`, i), caller)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("caller %d: status = %d: %s", i, rec.Code, rec.Body.String())
		}
	}
	for i, caller := range callers {
		filePath, err := memory.PurgatoryFilePath(caller.TenantID, caller.UserID, caller.KeyID)
		if err != nil {
			t.Fatal(err)
		}
		got, err := client.ReadFile(t.Context(), filePath)
		if err != nil {
			t.Fatal(err)
		}
		for j := range callers {
			if strings.Contains(got, fmt.Sprintf("private-note-%d", j)) != (i == j) {
				t.Errorf("caller %d journal has wrong visibility for caller %d", i, j)
			}
		}
	}
}

func TestObservationPurgatoryExcludedFromSharedMemory(t *testing.T) {
	handler, client := newTestHandler(t, false)
	if rec := post(handler, `{"project":"demo","kind":"lesson","content":"PURGATORY_ONLY_CONTENT"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	if rec := post(handler, `{"project":"demo","project_id":"project-demo","kind":"lesson","content":"CANONICAL_CONTENT"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	builder := memory.NewBuilder(client, memory.BundleConfig{}, nil, zerolog.Nop())
	bundle := builder.Build(t.Context(), "tenant-1--project-demo", "PURGATORY_ONLY_CONTENT", 4000, true)
	if !strings.Contains(bundle.Markdown, "CANONICAL_CONTENT") || strings.Contains(bundle.Markdown, "PURGATORY_ONLY_CONTENT") {
		t.Fatalf("purgatory contaminated a shared bundle: %s", bundle.Markdown)
	}
	got, err := client.ReadFile(t.Context(), memory.LessonsFilePath("tenant-1--project-demo"))
	if err != nil || strings.Contains(got, "PURGATORY_ONLY_CONTENT") {
		t.Fatalf("purgatory was implicitly promoted: %s, err=%v", got, err)
	}
}

func TestObservationPurgatoryConcurrentAppends(t *testing.T) {
	handler, client := newTestHandler(t, false)
	const count = 20
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			rec := post(handler, fmt.Sprintf(`{"project":"demo","kind":"lesson","content":"note-%d"}`, i))
			if rec.Code != http.StatusAccepted {
				t.Errorf("status = %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	wg.Wait()
	filePath, err := memory.PurgatoryFilePath("tenant-1", "user-1", "key-1")
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ReadFile(t.Context(), filePath)
	if err != nil {
		t.Fatal(err)
	}
	if entries := memory.ParseJournalEntries(got); len(entries) != count {
		t.Fatalf("stored %d observations, want %d", len(entries), count)
	}
}

func TestObservationStillRequiresAuthAndValidProjectID(t *testing.T) {
	handler, client := newTestHandler(t, false)
	for _, caller := range []*auth.Context{nil, {UserID: "user-1", KeyID: "key-1"}} {
		rec := postAs(handler, `{"project":"demo","kind":"lesson","content":"x"}`, caller)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("missing auth: status = %d, want 401", rec.Code)
		}
	}
	for _, projectID := range []string{"unknown-project", "foreign-project"} {
		rec := post(handler, fmt.Sprintf(`{"project":"demo","project_id":%q,"kind":"lesson","content":"x"}`, projectID))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("invalid project_id: status = %d, want 400", rec.Code)
		}
	}
	if _, err := client.ListDir(t.Context(), "purgatory"); !errors.Is(err, duffel.ErrNotFound) {
		t.Fatalf("invalid requests wrote purgatory: %v", err)
	}
}
