package webui

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"

	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	"github.com/earlgrey/piproxy/internal/webauth"
)

type logoutSessionStore struct {
	fail    bool
	revoked bool
}

func (*logoutSessionStore) Insert(context.Context, *pgstore.WebSession, []byte) error { return nil }
func (*logoutSessionStore) Touch(context.Context, string) error                       { return nil }
func (s *logoutSessionStore) GetByTokenHash(context.Context, []byte, time.Duration) (*pgstore.WebSession, error) {
	if s.revoked {
		return nil, pgstore.ErrSessionNotFound
	}
	return &pgstore.WebSession{ID: "s", TenantID: "t", UserID: "u", CSRFToken: "csrf", ExpiresAt: time.Now().Add(time.Hour)}, nil
}
func (s *logoutSessionStore) Revoke(context.Context, string) error {
	if s.fail {
		return errors.New("revocation unavailable")
	}
	s.revoked = true
	return nil
}

func TestOIDCLogoutFailureIsNotReportedAsSuccess(t *testing.T) {
	store := &logoutSessionStore{fail: true}
	manager := webauth.NewSessionManager(nil, keyLoginTenant{}, keyLoginUser{}, store, time.Hour, 0)
	handler := NewRouter(&Dependencies{Config: Config{Enabled: true}, Logger: zerolog.Nop(), Sessions: manager, OIDC: &webauth.Provider{}})
	logout := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/auth/logout", nil)
		r.AddCookie(&http.Cookie{Name: webauth.SessionCookie, Value: "opaque-oidc-session"})
		r.Header.Set(webauth.CSRFHeader, "csrf")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	if w := logout(); w.Code != http.StatusServiceUnavailable || len(w.Result().Cookies()) != 0 || store.revoked {
		t.Fatalf("failed revocation claimed sign-out: %d %s", w.Code, w.Body)
	}
	store.fail = false
	if w := logout(); w.Code != 200 || !store.revoked || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout retry failed: %d %s", w.Code, w.Body)
	}
}
