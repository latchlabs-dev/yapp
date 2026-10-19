package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/secrets"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	"github.com/earlgrey/piproxy/internal/webauth"
)

type keyLoginStore struct{ key *pgstore.KeyRecord }

func (s *keyLoginStore) GetByPrefix(context.Context, string) (*pgstore.KeyRecord, error) {
	return s.key, nil
}

type keyLoginTenant struct{}

func (keyLoginTenant) GetByID(context.Context, string) (*pgstore.TenantRecord, error) {
	return &pgstore.TenantRecord{ID: "t", Status: "active"}, nil
}

type keyLoginUser struct{}

func (keyLoginUser) GetByID(context.Context, string, string) (*pgstore.UserRecord, error) {
	return &pgstore.UserRecord{ID: "u", TenantID: "t", Email: "test@example.com", Status: "active"}, nil
}
func (keyLoginUser) GetByEmail(context.Context, string, string) (*pgstore.UserRecord, error) {
	return nil, nil
}
func (keyLoginUser) Insert(context.Context, *pgstore.UserRecord) error { return nil }

func keyLoginFixture(t *testing.T, origins ...string) (http.Handler, string, *keyLoginStore) {
	t.Helper()
	raw, prefix, hash, err := auth.GenerateKey("")
	if err != nil {
		t.Fatal(err)
	}
	store := &keyLoginStore{key: &pgstore.KeyRecord{ID: "k", TenantID: "t", UserID: "u", Prefix: prefix, KeyHash: hash, Status: "active", Scopes: []string{auth.ScopeLLMModelsRead}}}
	cipher, err := secrets.NewEncryptor([]byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatal(err)
	}
	keys := webauth.NewKeySessions(store, keyLoginTenant{}, keyLoginUser{}, nil, cipher, time.Hour)
	sessions := webauth.NewSessionManager(nil, keyLoginTenant{}, keyLoginUser{}, nil, time.Hour, 0)
	sessions.EnableKeyLogin(keys)
	origin := "https://console.test"
	if len(origins) > 0 {
		origin = origins[0]
	}
	return NewRouter(&Dependencies{Config: Config{Enabled: true, BaseURL: origin}, Logger: zerolog.Nop(), Sessions: sessions, KeySessions: keys, Tenants: keyLoginTenant{}, Users: keyLoginUser{}}), raw, store
}

func TestKeyLoginWithoutOIDC(t *testing.T) {
	handler, raw, store := keyLoginFixture(t)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/auth/config", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"api_key_enabled":true`) || !strings.Contains(rec.Body.String(), `"oidc_enabled":false`) {
		t.Fatalf("key-only portal unavailable: %d %s", rec.Code, rec.Body)
	}

	req := httptest.NewRequest("POST", "/auth/key", strings.NewReader(`{"key":"`+raw+`"}`))
	req.Header.Set("Origin", "https://console.test")
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || strings.Contains(rec.Body.String(), raw) {
		t.Fatalf("key login failed/leaked: %d", rec.Code)
	}
	var cookie *http.Cookie
	for _, candidate := range rec.Result().Cookies() {
		if candidate.Name == webauth.SessionCookie {
			cookie = candidate
		}
	}
	if cookie == nil || cookie.Value == raw || !cookie.Secure || !cookie.HttpOnly || cookie.Domain != "" || cookie.Path != "/" {
		t.Fatal("missing secure host-prefixed encrypted cookie")
	}
	req = httptest.NewRequest("GET", "/me", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var me struct {
		CSRF   string   `json:"csrf_token"`
		Scopes []string `json:"scopes"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &me) != nil || me.CSRF == "" || len(me.Scopes) != 1 {
		t.Fatalf("session not restored: %d %s", rec.Code, rec.Body)
	}

	for _, csrf := range []string{"", "wrong", me.CSRF} {
		req = httptest.NewRequest("POST", "/auth/logout", nil)
		req.AddCookie(cookie)
		req.Header.Set(webauth.CSRFHeader, csrf)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if csrf != me.CSRF {
			if rec.Code != 403 || len(rec.Result().Cookies()) != 0 {
				t.Fatal("logout bypassed CSRF")
			}
		} else if rec.Code != 200 || rec.Result().Cookies()[0].MaxAge != -1 {
			t.Fatal("logout failed to clear cookie")
		}
	}
	if store.key.Status != "active" {
		t.Fatal("logout revoked underlying key")
	}
	// Missing scopes still deny mutations, even with a valid CSRF token.
	req = httptest.NewRequest("PUT", "/projects/example/memory", strings.NewReader(`{"content":"x"}`))
	req.AddCookie(cookie)
	req.Header.Set(webauth.CSRFHeader, me.CSRF)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("key scopes were escalated")
	}
	store.key.Status = "revoked"
	req = httptest.NewRequest("GET", "/me", nil)
	req.AddCookie(cookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatal("key revocation did not invalidate cookie")
	}
}

func TestKeyLoginCanonicalOrigins(t *testing.T) {
	for _, tc := range []struct {
		config, presented string
		status            int
	}{
		{"https://CONSOLE.TEST:443/", "https://console.test", 200},
		{"https://console.test", "https://CONSOLE.TEST:443", 200},
		{"https://console.test:3335", "https://console.test", 403},
		{"https://console.test", "http://console.test", 403},
		{"https://console.test", "https://other.console.test", 403},
		{"https://console.test?bad", "https://console.test", 403},
	} {
		t.Run(tc.config+" vs "+tc.presented, func(t *testing.T) {
			handler, raw, _ := keyLoginFixture(t, tc.config)
			r := httptest.NewRequest("POST", "/auth/key", strings.NewReader(`{"key":"`+raw+`"}`))
			r.Header.Set("Origin", tc.presented)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("got %d, want %d", w.Code, tc.status)
			}
		})
	}
}

func TestKeyLoginRejectsUnsafeRequests(t *testing.T) {
	handler, raw, _ := keyLoginFixture(t)
	for _, tc := range []struct {
		name, origin, mediaType, body string
		status                        int
	}{
		{"missing origin", "", "application/json", `{"key":"` + raw + `"}`, 403},
		{"cross origin", "https://evil.test", "application/json", `{"key":"` + raw + `"}`, 403},
		{"null origin", "null", "application/json", `{"key":"` + raw + `"}`, 403},
		{"form", "https://console.test", "application/x-www-form-urlencoded", "key=x", 415},
		{"invalid JSON", "https://console.test", "application/json", "{", 400},
		{"unknown field", "https://console.test", "application/json", `{"key":"x","tenant_id":"other"}`, 400},
		{"trailing JSON", "https://console.test", "application/json", `{"key":"x"}{}`, 400},
		{"oversized", "https://console.test", "application/json", `{"key":"` + strings.Repeat("x", 5000) + `"}`, 400},
		{"wrong key", "https://console.test", "application/json", `{"key":"adm_not-a-proxy-key"}`, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/auth/key", strings.NewReader(tc.body))
			req.Header.Set("Origin", tc.origin)
			req.Header.Set("Content-Type", tc.mediaType)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.status || len(rec.Result().Cookies()) != 0 || strings.Contains(rec.Body.String(), raw) {
				t.Fatalf("unsafe login: status %d, want %d", rec.Code, tc.status)
			}
		})
	}
	req := httptest.NewRequest("POST", "/auth/key", strings.NewReader(`{"key":"`+raw+`"}`))
	req.Header.Set("Origin", "https://console.test")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatal("cross-site login metadata accepted")
	}

	for _, path := range []string{"/auth/login", "/auth/callback"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 404 {
			t.Fatalf("unconfigured OIDC route not disabled: %s = %d", path, rec.Code)
		}
	}
}
