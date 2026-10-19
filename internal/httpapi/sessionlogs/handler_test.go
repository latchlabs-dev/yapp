package sessionlogs

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeStore struct {
	hashes map[string]string
	record *pgstore.PiSessionLogRecord
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

func (f *fakeStore) ExistingHashes(_ context.Context, _, _ string, _ []string) (map[string]string, error) {
	return f.hashes, nil
}

func (f *fakeStore) Put(_ context.Context, record *pgstore.PiSessionLogRecord) error {
	f.record = record
	return nil
}

func TestStatusReturnsOnlyMissingOrChangedSessions(t *testing.T) {
	store := &fakeStore{hashes: map[string]string{
		"session-a": strings64("a"),
		"session-b": strings64("b"),
	}}
	handler := NewHandler(store, nil, zerolog.Nop())
	body := `{"sessions":[` +
		`{"session_id":"session-a","sha256":"` + strings64("a") + `"},` +
		`{"session_id":"session-b","sha256":"` + strings64("c") + `"},` +
		`{"session_id":"session-c","sha256":"` + strings64("d") + `"}]}`
	req := authenticatedRequest(http.MethodPost, "/proxy/v1/session-logs/status", bytes.NewBufferString(body))
	rec := httptest.NewRecorder()

	handler.Status(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var response StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	want := []string{"session-b", "session-c"}
	if len(response.RequiredSessionIDs) != len(want) {
		t.Fatalf("required = %v, want %v", response.RequiredSessionIDs, want)
	}
	for i := range want {
		if response.RequiredSessionIDs[i] != want[i] {
			t.Fatalf("required = %v, want %v", response.RequiredSessionIDs, want)
		}
	}
}

func TestUploadVerifiesAndStoresCanonicalSession(t *testing.T) {
	store := &fakeStore{}
	handler := NewHandler(store, nil, zerolog.Nop())
	raw := []byte("{\"type\":\"session\",\"version\":3,\"id\":\"session-a\",\"timestamp\":\"2026-07-10T10:00:00Z\",\"cwd\":\"/code/demo\"}\n" +
		"{\"type\":\"message\",\"id\":\"m1\",\"parentId\":null,\"timestamp\":\"2026-07-10T10:00:01Z\",\"message\":{\"role\":\"user\",\"content\":[{\"type\":\"text\",\"text\":\"hello\"}]}}\n")
	compressed := gzipBytes(t, raw)
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])

	req := authenticatedRequest(http.MethodPut, "/proxy/v1/session-logs/session-a", bytes.NewReader(compressed))
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("sessionID", "session-a")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Session-SHA256", hash)
	req.Header.Set("X-Session-File-Name", "session-a.jsonl")
	req.Header.Set("X-Session-Modified-Ms", "1783677601000")
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if store.record == nil {
		t.Fatal("session record was not stored")
	}
	if store.record.SessionID != "session-a" || store.record.Project != "demo" {
		t.Fatalf("record = %+v", store.record)
	}
	if store.record.EntryCount != 1 || store.record.ContentSHA256 != hash {
		t.Fatalf("record metadata = %+v", store.record)
	}
	if store.record.OwnerID != "user:user-1" {
		t.Fatalf("owner = %q", store.record.OwnerID)
	}
}

func TestUploadUsesResolvedProjectIDHeader(t *testing.T) {
	store := &fakeStore{}
	projects := &fakeProjectResolver{project: &pgstore.MemoryProject{ID: "mprj_shared", Slug: "example-app"}}
	handler := NewHandler(store, projects, zerolog.Nop())
	raw := []byte("{\"type\":\"session\",\"version\":3,\"id\":\"session-shared\",\"cwd\":\"/tmp/checkout\"}\n")
	sum := sha256.Sum256(raw)
	req := authenticatedRequest(http.MethodPut, "/proxy/v1/session-logs/session-shared", bytes.NewReader(gzipBytes(t, raw)))
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("sessionID", "session-shared")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Session-SHA256", hex.EncodeToString(sum[:]))
	req.Header.Set("X-Project-ID", "mprj_shared")
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if store.record.ProjectID != "mprj_shared" || store.record.Project != "example-app" {
		t.Fatalf("record = %+v", store.record)
	}
}

func TestUploadRejectsHashMismatch(t *testing.T) {
	handler := NewHandler(&fakeStore{}, nil, zerolog.Nop())
	raw := []byte("{\"type\":\"session\",\"version\":3,\"id\":\"session-a\"}\n")
	req := authenticatedRequest(http.MethodPut, "/proxy/v1/session-logs/session-a", bytes.NewReader(gzipBytes(t, raw)))
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("sessionID", "session-a")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	req.Header.Set("Content-Encoding", "gzip")
	req.Header.Set("X-Session-SHA256", strings64("a"))
	rec := httptest.NewRecorder()

	handler.Upload(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func authenticatedRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	authCtx := &auth.Context{TenantID: "tenant-1", UserID: "user-1", KeyID: "key-1"}
	return req.WithContext(middleware.WithAuthContext(req.Context(), authCtx))
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func strings64(ch string) string {
	return strings.Repeat(ch, 64)
}
