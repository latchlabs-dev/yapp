package webauth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"github.com/earlgrey/piproxy/internal/webauth"
)

// fakeIssuer is a minimal OIDC provider: discovery, JWKS, and a token endpoint
// that mints whatever ID token the test asks for. It lets the whole login flow
// run for real — discovery, PKCE, signature verification, nonce binding —
// without reaching Zitadel.
type fakeIssuer struct {
	server   *httptest.Server
	key      *rsa.PrivateKey
	clientID string

	// claims is mutated per-test to shape the ID token the token endpoint mints.
	claims map[string]any
	// nonce is echoed into the ID token; tests override it to simulate a
	// provider returning a token bound to a different login attempt.
	nonceOverride string
	lastNonce     string
}

func newFakeIssuer(t *testing.T, clientID string) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	f := &fakeIssuer{key: key, clientID: clientID}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, map[string]any{
			"issuer":                                f.server.URL,
			"authorization_endpoint":                f.server.URL + "/auth",
			"token_endpoint":                        f.server.URL + "/token",
			"jwks_uri":                              f.server.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(w, jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key:       key.Public(),
			KeyID:     "test",
			Algorithm: string(jose.RS256),
			Use:       "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		nonce := f.lastNonce
		if f.nonceOverride != "" {
			nonce = f.nonceOverride
		}
		writeTestJSON(w, map[string]any{
			"access_token": "at",
			"token_type":   "Bearer",
			"id_token":     f.mint(t, nonce),
		})
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeIssuer) mint(t *testing.T, nonce string) string {
	t.Helper()
	claims := map[string]any{
		"iss":            f.server.URL,
		"aud":            f.clientID,
		"sub":            "subject-1",
		"iat":            time.Now().Add(-time.Minute).Unix(),
		"exp":            time.Now().Add(time.Hour).Unix(),
		"nonce":          nonce,
		"email":          "dev@example.com",
		"email_verified": true,
		"name":           "Dev Example",
	}
	for k, v := range f.claims {
		claims[k] = v
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	signed, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	token, err := signed.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return token
}

func writeTestJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

func newTestProvider(t *testing.T, issuer *fakeIssuer) *webauth.Provider {
	t.Helper()
	provider, err := webauth.NewProvider(context.Background(), webauth.Config{
		Issuer:      issuer.server.URL,
		ClientID:    issuer.clientID,
		RedirectURL: "https://console.example.com/web/v1/auth/callback",
		Cookie:      webauth.CookieOptions{Insecure: true},
	})
	if err != nil {
		t.Fatalf("new provider: %v", err)
	}
	return provider
}

// startLogin runs the redirect leg and returns the state, the nonce the
// provider must echo, and the login cookie to replay on the callback.
func startLogin(t *testing.T, provider *webauth.Provider, issuer *fakeIssuer, returnTo string) (state string, cookie *http.Cookie) {
	t.Helper()
	rec := httptest.NewRecorder()
	authURL, err := provider.StartLogin(rec, returnTo)
	if err != nil {
		t.Fatalf("start login: %v", err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	q := parsed.Query()
	if q.Get("code_challenge") == "" || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("expected PKCE S256 challenge, got %v", q)
	}
	issuer.lastNonce = q.Get("nonce")

	for _, c := range rec.Result().Cookies() {
		if c.Name == webauth.LoginCookieName(webauth.CookieOptions{Insecure: true}) {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("expected a login cookie to be set")
	}
	return q.Get("state"), cookie
}

func callback(t *testing.T, provider *webauth.Provider, state string, cookie *http.Cookie) (*webauth.Claims, string, error) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/web/v1/auth/callback?code=abc&state="+url.QueryEscape(state), nil)
	if cookie != nil {
		r.AddCookie(cookie)
	}
	return provider.CompleteLogin(context.Background(), httptest.NewRecorder(), r)
}

func TestCompleteLoginSucceeds(t *testing.T) {
	issuer := newFakeIssuer(t, "console")
	provider := newTestProvider(t, issuer)

	state, cookie := startLogin(t, provider, issuer, "/c/abc")
	claims, returnTo, err := callback(t, provider, state, cookie)
	if err != nil {
		t.Fatalf("complete login: %v", err)
	}
	if claims.Email != "dev@example.com" {
		t.Errorf("email = %q, want dev@example.com", claims.Email)
	}
	if claims.Subject != "subject-1" {
		t.Errorf("subject = %q, want subject-1", claims.Subject)
	}
	if claims.Name != "Dev Example" {
		t.Errorf("name = %q, want Dev Example", claims.Name)
	}
	if returnTo != "/c/abc" {
		t.Errorf("returnTo = %q, want /c/abc", returnTo)
	}
}

// A callback whose state does not match the cookie is either a stale tab or a
// login-CSRF attempt, and must not produce a session either way.
func TestCompleteLoginRejectsStateMismatch(t *testing.T) {
	issuer := newFakeIssuer(t, "console")
	provider := newTestProvider(t, issuer)

	_, cookie := startLogin(t, provider, issuer, "")
	if _, _, err := callback(t, provider, "not-the-state", cookie); !errors.Is(err, webauth.ErrLoginStateMismatch) {
		t.Fatalf("err = %v, want ErrLoginStateMismatch", err)
	}
}

// Without the login cookie there is no PKCE verifier and no expected nonce, so
// a replayed authorization code is useless.
func TestCompleteLoginRejectsMissingCookie(t *testing.T) {
	issuer := newFakeIssuer(t, "console")
	provider := newTestProvider(t, issuer)

	state, _ := startLogin(t, provider, issuer, "")
	if _, _, err := callback(t, provider, state, nil); err == nil {
		t.Fatal("expected an error when the login cookie is absent")
	}
}

// A token minted for a different login attempt must be refused even though its
// signature is valid — that is the whole point of the nonce.
func TestCompleteLoginRejectsNonceMismatch(t *testing.T) {
	issuer := newFakeIssuer(t, "console")
	provider := newTestProvider(t, issuer)

	state, cookie := startLogin(t, provider, issuer, "")
	issuer.nonceOverride = "some-other-attempt"
	if _, _, err := callback(t, provider, state, cookie); !errors.Is(err, webauth.ErrLoginStateMismatch) {
		t.Fatalf("err = %v, want ErrLoginStateMismatch", err)
	}
}

func TestCompleteLoginRejectsExpiredToken(t *testing.T) {
	issuer := newFakeIssuer(t, "console")
	provider := newTestProvider(t, issuer)
	issuer.claims = map[string]any{"exp": time.Now().Add(-time.Hour).Unix()}

	state, cookie := startLogin(t, provider, issuer, "")
	_, _, err := callback(t, provider, state, cookie)
	if err == nil || !strings.Contains(err.Error(), "verify id_token") {
		t.Fatalf("err = %v, want an id_token verification failure", err)
	}
}

// Users are provisioned from the email claim, so an unverified address would
// let anyone who can assert it claim another person's account.
func TestCompleteLoginRejectsUnverifiedEmail(t *testing.T) {
	issuer := newFakeIssuer(t, "console")
	provider := newTestProvider(t, issuer)
	issuer.claims = map[string]any{"email_verified": false}

	state, cookie := startLogin(t, provider, issuer, "")
	if _, _, err := callback(t, provider, state, cookie); !errors.Is(err, webauth.ErrEmailNotVerified) {
		t.Fatalf("err = %v, want ErrEmailNotVerified", err)
	}
}

// A provider that omits email_verified entirely is treated as unverified
// rather than trusted by default.
func TestCompleteLoginRejectsAbsentEmailVerified(t *testing.T) {
	issuer := newFakeIssuer(t, "console")
	provider := newTestProvider(t, issuer)
	issuer.claims = map[string]any{"email_verified": nil}

	state, cookie := startLogin(t, provider, issuer, "")
	if _, _, err := callback(t, provider, state, cookie); !errors.Is(err, webauth.ErrEmailNotVerified) {
		t.Fatalf("err = %v, want ErrEmailNotVerified", err)
	}
}

// The login cookie must be cleared on every path, so a captured cookie cannot
// be replayed against a second callback.
func TestCompleteLoginClearsLoginCookie(t *testing.T) {
	issuer := newFakeIssuer(t, "console")
	provider := newTestProvider(t, issuer)

	state, cookie := startLogin(t, provider, issuer, "")
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/web/v1/auth/callback?code=abc&state="+url.QueryEscape(state), nil)
	r.AddCookie(cookie)
	if _, _, err := provider.CompleteLogin(context.Background(), rec, r); err != nil {
		t.Fatalf("complete login: %v", err)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == webauth.LoginCookieName(webauth.CookieOptions{Insecure: true}) && c.MaxAge >= 0 {
			t.Fatalf("login cookie was not expired: MaxAge = %d", c.MaxAge)
		}
	}
}

func TestHashTokenIsStable(t *testing.T) {
	token, err := webauth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	first := webauth.HashToken(token)
	second := webauth.HashToken(token)
	if len(first) != sha256.Size {
		t.Fatalf("hash length = %d, want %d", len(first), sha256.Size)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("hashing the same token twice produced different values")
	}
	other, err := webauth.NewToken()
	if err != nil {
		t.Fatalf("new token: %v", err)
	}
	if bytes.Equal(first, webauth.HashToken(other)) {
		t.Fatal("distinct tokens hashed to the same value")
	}
}
