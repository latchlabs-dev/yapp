package codexauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/secrets"
)

func TestRefresh_Success(t *testing.T) {
	var gotForm string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		gotForm = r.PostForm.Encode()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token":"new-access",
			"token_type":"Bearer",
			"expires_in":864000,
			"scope":"openid profile email offline_access",
			"id_token":"idt",
			"refresh_token":"new-refresh",
			"earliest_refresh_at":"2026-07-02T00:00:00Z"
		}`))
	}))
	t.Cleanup(srv.Close)

	rf := NewRefresher()
	rf.client = srv.Client()

	tr, err := rf.Refresh(context.Background(), "old-refresh", srv.URL, "test-client", "test-scope")
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if tr.AccessToken != "new-access" {
		t.Errorf("access_token = %q, want new-access", tr.AccessToken)
	}
	if tr.RefreshToken != "new-refresh" {
		t.Errorf("refresh_token = %q, want new-refresh (rotation)", tr.RefreshToken)
	}
	if tr.ExpiresIn != 864000 {
		t.Errorf("expires_in = %d, want 864000", tr.ExpiresIn)
	}
	if !strings.Contains(gotForm, "grant_type=refresh_token") ||
		!strings.Contains(gotForm, "client_id=test-client") ||
		!strings.Contains(gotForm, "scope=test-scope") ||
		!strings.Contains(gotForm, "refresh_token=old-refresh") {
		t.Errorf("unexpected form body: %s", gotForm)
	}
}

func TestRefresh_AcceptsNumericEarliestRefreshAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token":"new-access",
			"refresh_token":"new-refresh",
			"expires_in":864000,
			"earliest_refresh_at":1788418800
		}`))
	}))
	t.Cleanup(srv.Close)

	rf := NewRefresher()
	rf.client = srv.Client()

	tr, err := rf.Refresh(context.Background(), "old-refresh", srv.URL, "test-client", "")
	if err != nil {
		t.Fatalf("refresh with numeric earliest_refresh_at: %v", err)
	}
	if got := string(tr.EarliestRefreshAt); got != "1788418800" {
		t.Errorf("earliest_refresh_at = %q, want numeric value retained", got)
	}
}

func TestRefresh_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	t.Cleanup(srv.Close)

	rf := NewRefresher()
	rf.client = srv.Client()

	if _, err := rf.Refresh(context.Background(), "bad", srv.URL, "test-client", ""); err == nil {
		t.Fatal("expected error on non-200 response")
	}
}

func TestRefresh_MissingTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"only-access"}`))
	}))
	t.Cleanup(srv.Close)

	rf := NewRefresher()
	rf.client = srv.Client()

	if _, err := rf.Refresh(context.Background(), "rt", srv.URL, "test-client", ""); err == nil {
		t.Fatal("expected error when refresh_token missing from response")
	}
}

func TestRefreshCredential_RotatesAndSetsExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"A2","refresh_token":"R2","expires_in":3600,"token_type":"Bearer"}`))
	}))
	t.Cleanup(srv.Close)

	rf := NewRefresher()
	rf.client = srv.Client()

	before := &secrets.Credential{
		AccessToken:   "A1",
		RefreshToken:  "R1",
		AccountID:     "acct-123",
		OAuthTokenURL: srv.URL,
		OAuthClientID: "test-client",
		OAuthScope:    "test-scope",
		ExpiresAt:     time.Now().Add(-time.Minute), // expired
	}
	after, err := rf.RefreshCredential(context.Background(), before)
	if err != nil {
		t.Fatalf("refresh credential: %v", err)
	}
	if after.AccessToken != "A2" || after.RefreshToken != "R2" {
		t.Errorf("tokens not rotated: %+v", after)
	}
	if after.AccountID != "acct-123" {
		t.Errorf("account id not preserved: %q", after.AccountID)
	}
	if time.Until(after.ExpiresAt) <= 3500*time.Second {
		t.Errorf("expiry not set from expires_in: %v", after.ExpiresAt)
	}
	// Original credential must be untouched (rotation is non-destructive).
	if before.RefreshToken != "R1" {
		t.Errorf("original credential mutated: %q", before.RefreshToken)
	}
}

func TestRefresh_EmptyToken(t *testing.T) {
	rf := NewRefresher()
	if _, err := rf.Refresh(context.Background(), "  ", "https://example.test/token", "test-client", ""); err == nil {
		t.Fatal("expected error for empty refresh token")
	}
}

func TestRefreshCredential_RequiresDBOAuthConfig(t *testing.T) {
	rf := NewRefresher()
	_, err := rf.RefreshCredential(context.Background(), &secrets.Credential{
		RefreshToken: "rt",
	})
	if err == nil {
		t.Fatal("expected error for missing DB-backed OAuth config")
	}
}
