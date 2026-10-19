package memorycontext

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/memory"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeProjectResolver struct{}

func (fakeProjectResolver) Resolve(_ context.Context, tenantID, _ string, _ []string) (*pgstore.MemoryProject, error) {
	return &pgstore.MemoryProject{ID: "mprj_shared", TenantID: tenantID, Slug: "example-app"}, nil
}

type unresolvedProjectResolver struct{}

func (unresolvedProjectResolver) Resolve(context.Context, string, string, []string) (*pgstore.MemoryProject, error) {
	return nil, nil
}

// mockDuffel creates a local duffellib workspace for builder integration.
func mockDuffel(t *testing.T, files map[string]string) *duffel.Client {
	t.Helper()
	client := duffel.New(duffel.Config{Root: t.TempDir()})
	for path, content := range files {
		if err := client.WriteFile(t.Context(), path, content); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	return client
}

func newHandler(t *testing.T, files map[string]string, hooks ...memory.Hook) http.HandlerFunc {
	t.Helper()
	builder := memory.NewBuilder(mockDuffel(t, files), memory.BundleConfig{}, nil, zerolog.Nop())
	chain := memory.NewHookChain(0, zerolog.Nop(), hooks...)
	return NewHandler(builder, chain, nil, nil, Config{DefaultBudget: 4000}, zerolog.Nop())
}

func doRequest(handler http.HandlerFunc, url string) (*httptest.ResponseRecorder, Response) {
	req := httptest.NewRequest(http.MethodGet, url, nil)
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tenant-1", UserID: "user-1", KeyID: "key-1",
	}))
	rec := httptest.NewRecorder()
	handler(rec, req)
	var resp Response
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec, resp
}

func TestContextReturnsRawBundleWhenEnabled(t *testing.T) {
	handler := newHandler(t, map[string]string{
		"projects/demo/memory.md": "# Facts\n\nUse pnpm.",
	})
	rec, resp := doRequest(handler, "/proxy/v1/context?project=demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !resp.Inject || resp.Bundle == "" || resp.TokenEstimate == 0 || resp.Degraded {
		t.Errorf("resp = %+v, want enabled raw bundle", resp)
	}
}

func TestContextReturnsStableRepositoryProjectID(t *testing.T) {
	builder := memory.NewBuilder(mockDuffel(t, nil), memory.BundleConfig{}, nil, zerolog.Nop())
	handler := NewHandler(builder, memory.NewHookChain(0, zerolog.Nop()), nil, fakeProjectResolver{}, Config{DefaultBudget: 4000}, zerolog.Nop())
	rec, resp := doRequest(handler, "/proxy/v1/context?project=checkout&repo=git@github.com:example-org/example-app.git")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if resp.ProjectID != "mprj_shared" || resp.Project != "example-app" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestContextWithoutRepositoryIdentityReturnsObservableEmptyResponse(t *testing.T) {
	var logs bytes.Buffer
	logger := zerolog.New(&logs).Level(zerolog.InfoLevel)
	builder := memory.NewBuilder(mockDuffel(t, nil), memory.BundleConfig{}, nil, logger)
	handler := NewHandler(
		builder,
		memory.NewHookChain(0, logger),
		nil,
		unresolvedProjectResolver{},
		Config{DefaultBudget: 4000},
		logger,
	)

	rec, resp := doRequest(handler, "/proxy/v1/context?project=admin")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if resp.Inject || resp.Bundle != "" || resp.ProjectID != "" {
		t.Fatalf("response = %+v, want unresolved empty response", resp)
	}
	if resp.Reason != "unresolved repository identity" {
		t.Fatalf("reason = %q", resp.Reason)
	}
	for _, want := range []string{
		`"message":"context bundle served"`,
		`"project":"admin"`,
		`"inject":false`,
		`"bundle_present":false`,
		`"reason":"unresolved repository identity"`,
		`"tokens":0`,
		`"degraded":false`,
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("info log missing %s: %s", want, logs.String())
		}
	}
}

func TestContextInvalidProject(t *testing.T) {
	handler := newHandler(t, nil)
	rec, _ := doRequest(handler, "/proxy/v1/context?project=..")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestContextHookDisablesInjection(t *testing.T) {
	off := memory.HookFunc{HookName: "off", Fn: func(ctx context.Context, req *memory.InjectionRequest, d *memory.Decision) error {
		d.Inject = false
		d.Reason = "test disabled"
		return nil
	}}
	handler := newHandler(t, map[string]string{
		"projects/demo/memory.md": "facts",
	}, off)

	rec, resp := doRequest(handler, "/proxy/v1/context?project=demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even when injection is off", rec.Code)
	}
	if resp.Inject || resp.Bundle != "" {
		t.Errorf("resp = %+v, want inject=false with empty bundle", resp)
	}
	if resp.Reason != "test disabled" {
		t.Errorf("reason = %q", resp.Reason)
	}
}

func TestContextReportsDegradedBundleWhenDuffelDown(t *testing.T) {
	builder := memory.NewBuilder(duffel.New(duffel.Config{}), memory.BundleConfig{}, nil, zerolog.Nop())
	handler := NewHandler(builder, memory.NewHookChain(0, zerolog.Nop()), nil, nil, Config{DefaultBudget: 4000}, zerolog.Nop())

	rec, resp := doRequest(handler, "/proxy/v1/context?project=demo")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !resp.Inject || resp.Bundle != "" || !resp.Degraded || resp.DegradedReason == "" {
		t.Errorf("resp = %+v, want degraded empty bundle", resp)
	}
}

func TestContextBudgetOverride(t *testing.T) {
	handler := newHandler(t, map[string]string{
		"projects/demo/memory.md": "facts",
	})
	rec, resp := doRequest(handler, "/proxy/v1/context?project=demo&budget=999999")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !resp.Inject || resp.Bundle == "" {
		t.Errorf("resp = %+v, want query budget ignored and default bundle returned", resp)
	}
}

func TestContextExplicitRetrievalBypassesInjectionPolicy(t *testing.T) {
	storageProject, err := memory.ScopedProject("tenant-1", "mprj_shared")
	if err != nil {
		t.Fatal(err)
	}
	builder := memory.NewBuilder(mockDuffel(t, map[string]string{
		memory.FilePath(storageProject): "# Project memory: example-app\n\nUse pnpm.",
	}), memory.BundleConfig{}, nil, zerolog.Nop())
	off := memory.NewGlobalToggleHook(func() bool { return false })
	handler := NewHandler(builder, memory.NewHookChain(0, zerolog.Nop(), off), nil, fakeProjectResolver{}, Config{DefaultBudget: 4000}, zerolog.Nop())

	body := `{"project":"checkout","repo":"git@github.com:example-org/example-app.git","retrieve":true,"tools":true}`
	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/context", strings.NewReader(body))
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tenant-1", UserID: "user-1", KeyID: "key-1",
	}))
	rec := httptest.NewRecorder()
	handler(rec, req)
	var resp Response
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)

	if rec.Code != http.StatusOK || resp.Inject || !strings.Contains(resp.Bundle, "Use pnpm") {
		t.Fatalf("status=%d resp=%+v, want on-demand bundle without automatic injection", rec.Code, resp)
	}
	if resp.Reason != "retrieved on demand" {
		t.Errorf("reason = %q", resp.Reason)
	}
	if !strings.Contains(resp.Bundle, "record_memory") {
		t.Error("tool-capable retrieved bundle should mention record_memory")
	}
}
