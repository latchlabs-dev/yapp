// Package webauth implements existing-key and optional OIDC authentication for
// the private web portal, including encrypted cookies and browser sessions.
package webauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

const (
	// SessionCookie carries either an opaque OIDC session token (stored only
	// as a hash in Postgres) or an encrypted, expiring existing-key envelope.
	SessionCookie = "__Host-piproxy_web_session"

	// LoginCookie carries the in-flight OIDC login state (state, nonce, PKCE
	// verifier) between the redirect and the callback.
	LoginCookie = "__Host-piproxy_web_login"

	// CSRFHeader must accompany every state-changing web request. The session
	// cookie is SameSite=Lax, which stops cross-site POSTs on its own; this is
	// the second, explicit check.
	CSRFHeader = "X-Web-CSRF"

	// tokenBytes is the entropy in a session token. 32 bytes matches the proxy
	// API key secret length in internal/auth.
	tokenBytes = 32
)

// NewToken returns a fresh URL-safe session token.
func NewToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate session token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// HashToken returns the storage form of a session token. Plain SHA-256 with no
// pepper is deliberate: the token is 256 bits of uniform randomness, so it is
// not brute-forceable and needs no stretching.
func HashToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// CookieOptions carries the deployment-specific parts of cookie writing.
type CookieOptions struct {
	// Domain is used only to expire a configured legacy cookie. New cookies
	// are always host-only; runtime config forbids a shared cookie domain.
	Domain string
	// Insecure drops the Secure attribute. Only for local http:// development;
	// production always serves the console over TLS.
	Insecure bool
}

// NewCSRFToken returns a token to hand the SPA, which echoes it back in
// CSRFHeader on every state-changing request.
func NewCSRFToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate csrf token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
