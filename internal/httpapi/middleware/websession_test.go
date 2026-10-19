package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	"github.com/earlgrey/piproxy/internal/webauth"
)

type fixedWebSession struct{ session *pgstore.WebSession }

func (s fixedWebSession) Resolve(context.Context, string) (*pgstore.WebSession, error) {
	return s.session, nil
}

func TestWebSessionPreservesKeyIdentityAndChecksCSRF(t *testing.T) {
	sess := &pgstore.WebSession{TenantID: "tenant", UserID: "user", KeyID: "key", KeyPrefix: "prefix", Scopes: []string{"llm:invoke"}, CSRFToken: "csrf", ExpiresAt: time.Now().Add(time.Hour)}
	handler := WebSession(fixedWebSession{sess}, webauth.CookieOptions{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := GetAuthContext(r.Context())
		if ctx.KeyID != sess.KeyID || ctx.KeyPrefix != sess.KeyPrefix || ctx.TenantID != sess.TenantID || !ctx.KeyExpires.Equal(sess.ExpiresAt) {
			t.Fatal("key quota/usage identity lost")
		}
		w.WriteHeader(204)
	}))
	for _, tc := range []struct {
		method, csrf string
		status       int
	}{
		{"GET", "", 204}, {"POST", "csrf", 204}, {"POST", "", 403}, {"DELETE", "wrong", 403},
	} {
		req := httptest.NewRequest(tc.method, "/web/v1/test", nil)
		req.AddCookie(&http.Cookie{Name: webauth.SessionCookie, Value: "sealed"})
		req.Header.Set(webauth.CSRFHeader, tc.csrf)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.status {
			t.Errorf("%s csrf=%q: %d, want %d", tc.method, tc.csrf, rec.Code, tc.status)
		}
	}
	// A missing server-side CSRF value must never match an empty header.
	sess.CSRFToken = ""
	req := httptest.NewRequest("POST", "/web/v1/test", nil)
	req.AddCookie(&http.Cookie{Name: webauth.SessionCookie, Value: "sealed"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("empty CSRF accepted")
	}
}
