package sessionsubmit

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeSessionStore struct {
	record *pgstore.SessionSubmissionRecord
	err    error
}

type fakeProjectResolver struct {
	project *pgstore.MemoryProject
}

func (f *fakeProjectResolver) Resolve(_ context.Context, _, _ string, _ []string) (*pgstore.MemoryProject, error) {
	return f.project, nil
}

func (f *fakeProjectResolver) GetForTenant(_ context.Context, _, _ string) (*pgstore.MemoryProject, error) {
	return f.project, nil
}

func (f *fakeSessionStore) Put(_ context.Context, record *pgstore.SessionSubmissionRecord, _ []byte) error {
	f.record = record
	return f.err
}

func TestSessionSubmissionStoresAndReturnsImmediately(t *testing.T) {
	store := &fakeSessionStore{}
	handler := NewHandler(store, nil, nil, zerolog.Nop())
	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/session-submit", bytes.NewBufferString(`{
		"conversation_id":"conv-nightly",
		"project":"Demo",
		"message_count":4,
		"ended_at":"2026-07-10T22:00:00Z"
	}`))
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tenant-1", UserID: "user-1", KeyID: "key-1",
	}))
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if store.record == nil || store.record.Project != "demo" {
		t.Fatalf("stored record = %+v", store.record)
	}
	if store.record.TenantID != "tenant-1" || store.record.OwnerID != "user:user-1" {
		t.Fatalf("stored ownership = %+v", store.record)
	}
}

func TestSessionSubmissionStoresResolvedRepositoryProject(t *testing.T) {
	store := &fakeSessionStore{}
	projects := &fakeProjectResolver{project: &pgstore.MemoryProject{ID: "mprj_shared", Slug: "example-app"}}
	handler := NewHandler(store, projects, nil, zerolog.Nop())
	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/session-submit", bytes.NewBufferString(`{
		"conversation_id":"conv-shared",
		"project":"local-checkout-name",
		"repo":"git@github.com:example-org/example-app.git"
	}`))
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tenant-1", UserID: "user-1", KeyID: "key-1",
	}))
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if store.record.ProjectID != "mprj_shared" || store.record.Project != "example-app" {
		t.Fatalf("stored record = %+v", store.record)
	}
}

func TestSessionSubmissionNormalizesAttribution(t *testing.T) {
	store := &fakeSessionStore{}
	handler := NewHandler(store, nil, nil, zerolog.Nop())
	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/session-submit", bytes.NewBufferString(`{
		"conversation_id":"conv-attrib",
		"message_count":2,
		"thinking_level":"HIGH",
		"session_name":"  Refactor auth module  ",
		"first_prompt":"fix the login bug",
		"run_mode":"tui"
	}`))
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tenant-1", UserID: "user-1", KeyID: "key-1",
	}))
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	r := store.record
	if r.ThinkingLevel != "high" {
		t.Errorf("thinking_level = %q, want %q", r.ThinkingLevel, "high")
	}
	if r.SessionName != "Refactor auth module" {
		t.Errorf("session_name = %q, want trimmed name", r.SessionName)
	}
	if r.FirstPrompt != "fix the login bug" {
		t.Errorf("first_prompt = %q, want passthrough", r.FirstPrompt)
	}
	if r.RunMode != "tui" {
		t.Errorf("run_mode = %q, want %q", r.RunMode, "tui")
	}
}

func TestSessionSubmissionDropsInvalidAttribution(t *testing.T) {
	store := &fakeSessionStore{}
	handler := NewHandler(store, nil, nil, zerolog.Nop())
	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/session-submit", bytes.NewBufferString(`{
		"conversation_id":"conv-junk",
		"message_count":1,
		"thinking_level":"ultra",
		"run_mode":"mainframe"
	}`))
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tenant-1", UserID: "user-1", KeyID: "key-1",
	}))
	rec := httptest.NewRecorder()

	handler(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if store.record.ThinkingLevel != "" {
		t.Errorf("thinking_level = %q, want dropped", store.record.ThinkingLevel)
	}
	if store.record.RunMode != "" {
		t.Errorf("run_mode = %q, want dropped", store.record.RunMode)
	}
}
