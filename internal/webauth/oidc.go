package webauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// ErrLoginStateMismatch is returned when the callback's state parameter does
// not match the value we planted in the login cookie. That is either a stale
// tab or a login-CSRF attempt; both are refused.
var ErrLoginStateMismatch = errors.New("login state mismatch")

// ErrEmailNotVerified is returned when the provider reports an unverified
// address. We provision users from the email claim, so an unverified address
// would let anyone who can assert it claim someone else's account.
var ErrEmailNotVerified = errors.New("email address is not verified by the identity provider")

// Config configures the OIDC client.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
	Cookie       CookieOptions
}

// Claims are the identity facts we take from a verified ID token. Nothing else
// from the token is used or stored.
type Claims struct {
	Issuer        string
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// Provider wraps the OIDC discovery document, token verifier, and OAuth2
// client for one identity provider.
type Provider struct {
	cfg      Config
	verifier *oidc.IDTokenVerifier
	oauth    oauth2.Config
}

// loginState is the short-lived per-attempt secret stored in the login cookie.
// It never reaches the server between the redirect and the callback, so the
// cookie is the only place it lives.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Return   string `json:"r,omitempty"`
}

// NewProvider performs OIDC discovery against the issuer. It is called at
// startup; a provider that is unreachable then is a fatal misconfiguration for
// the web console, not something to retry silently per request.
func NewProvider(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oidc issuer, client id, and redirect url are required")
	}
	provider, err := oidc.NewProvider(ctx, strings.TrimSuffix(cfg.Issuer, "/"))
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %s: %w", cfg.Issuer, err)
	}
	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "profile", "email"}
	}
	return &Provider{
		cfg:      cfg,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       scopes,
		},
	}, nil
}

// Issuer returns the configured issuer, used to key stored identities.
func (p *Provider) Issuer() string { return strings.TrimSuffix(p.cfg.Issuer, "/") }

// StartLogin plants the per-attempt login cookie and returns the provider URL
// to redirect the browser to. PKCE S256 is always used, so the flow is safe
// even when the client is configured as public (no secret).
func (p *Provider) StartLogin(w http.ResponseWriter, returnTo string) (string, error) {
	state, err := randomToken()
	if err != nil {
		return "", err
	}
	nonce, err := randomToken()
	if err != nil {
		return "", err
	}
	verifier, err := randomToken()
	if err != nil {
		return "", err
	}

	encoded, err := json.Marshal(loginState{State: state, Nonce: nonce, Verifier: verifier, Return: returnTo})
	if err != nil {
		return "", fmt.Errorf("encode login state: %w", err)
	}
	clearLegacyLoginCookies(w, p.cfg.Cookie)
	http.SetCookie(w, &http.Cookie{
		Name:     LoginCookieName(p.cfg.Cookie),
		Value:    base64.RawURLEncoding.EncodeToString(encoded),
		Path:     "/",
		MaxAge:   int((10 * time.Minute).Seconds()),
		HttpOnly: true,
		Secure:   !p.cfg.Cookie.Insecure,
		// Lax, not Strict: the provider sends the browser back with a
		// top-level GET, which Lax allows and Strict would drop.
		SameSite: http.SameSiteLaxMode,
	})

	challenge := sha256.Sum256([]byte(verifier))
	return p.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.SetAuthURLParam("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:])),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
	), nil
}

// CompleteLogin validates the callback, exchanges the code, and verifies the
// ID token. It clears the login cookie on every path, success or failure, so a
// stale attempt cannot be replayed.
func (p *Provider) CompleteLogin(ctx context.Context, w http.ResponseWriter, r *http.Request) (*Claims, string, error) {
	defer p.clearLoginCookie(w)

	st, err := p.readLoginState(r)
	if err != nil {
		return nil, "", err
	}
	if subtle.ConstantTimeCompare([]byte(st.State), []byte(r.URL.Query().Get("state"))) != 1 {
		return nil, "", ErrLoginStateMismatch
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		return nil, "", errors.New("authorization code missing from callback")
	}

	token, err := p.oauth.Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", st.Verifier),
	)
	if err != nil {
		return nil, "", fmt.Errorf("exchange authorization code: %w", err)
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok || rawID == "" {
		return nil, "", errors.New("identity provider returned no id_token")
	}
	idToken, err := p.verifier.Verify(ctx, rawID)
	if err != nil {
		return nil, "", fmt.Errorf("verify id_token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idToken.Nonce), []byte(st.Nonce)) != 1 {
		return nil, "", ErrLoginStateMismatch
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Name          string `json:"name"`
		PreferredName string `json:"preferred_username"`
	}
	if err := idToken.Claims(&claims); err != nil {
		return nil, "", fmt.Errorf("decode id_token claims: %w", err)
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" {
		return nil, "", errors.New("id_token carries no email claim")
	}
	// Treat an absent email_verified as unverified. Zitadel sets it; a provider
	// that does not is not one we can safely provision from.
	if claims.EmailVerified == nil || !*claims.EmailVerified {
		return nil, "", ErrEmailNotVerified
	}

	name := claims.Name
	if name == "" {
		name = claims.PreferredName
	}
	return &Claims{
		Issuer:        idToken.Issuer,
		Subject:       idToken.Subject,
		Email:         email,
		EmailVerified: true,
		Name:          name,
	}, st.Return, nil
}

func (p *Provider) readLoginState(r *http.Request) (*loginState, error) {
	value := readCookie(r, LoginCookieName(p.cfg.Cookie))
	if value == "" {
		return nil, errors.New("login cookie missing or expired")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, ErrLoginStateMismatch
	}
	st := &loginState{}
	if err := json.Unmarshal(raw, st); err != nil {
		return nil, ErrLoginStateMismatch
	}
	if st.State == "" || st.Nonce == "" || st.Verifier == "" {
		return nil, ErrLoginStateMismatch
	}
	return st, nil
}

func (p *Provider) clearLoginCookie(w http.ResponseWriter) {
	expireCookie(w, LoginCookieName(p.cfg.Cookie), "/", "", !p.cfg.Cookie.Insecure)
	clearLegacyLoginCookies(w, p.cfg.Cookie)
}

func randomToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
