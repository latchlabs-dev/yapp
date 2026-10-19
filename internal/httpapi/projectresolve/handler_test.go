package projectresolve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeStore struct {
	tenantID string
	remotes  []string
}

func (s *fakeStore) Resolve(_ context.Context, tenantID, slug string, remotes []string) (*pgstore.MemoryProject, error) {
	s.tenantID = tenantID
	s.remotes = remotes
	return &pgstore.MemoryProject{ID: "mprj_resolved", TenantID: tenantID, Slug: slug}, nil
}

func TestResolveUsesAuthenticatedTenantWithoutMemoryScope(t *testing.T) {
	store := &fakeStore{}
	handler := NewHandler(store)
	req := httptest.NewRequest(http.MethodPost, "/proxy/v1/projects/resolve", bytes.NewBufferString(`{
		"project":"piproxy","repo":"git@github.com:earlgrey/piproxy.git"
	}`))
	req = req.WithContext(middleware.WithAuthContext(req.Context(), &auth.Context{
		TenantID: "tn_test", KeyID: "key_test", Scopes: []string{"llm:invoke", "sessions:submit"},
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]string
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body["project_id"] != "mprj_resolved" || store.tenantID != "tn_test" || len(store.remotes) != 1 {
		t.Fatalf("body=%v tenant=%q remotes=%v", body, store.tenantID, store.remotes)
	}
}
