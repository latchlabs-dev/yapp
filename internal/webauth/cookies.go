package webauth

import (
	"net/http"
	"time"
)

const (
	DevSessionCookie    = "piproxy_web_session_dev"
	DevLoginCookie      = "piproxy_web_login_dev"
	legacySessionCookie = "piproxy_web_session"
	legacyLoginCookie   = "piproxy_web_login"
)

// Development names are accepted ONLY in explicit loopback HTTP mode. Never
// fall back to them or to legacy names in production: a sibling domain can
// plant an ordinary cookie but browsers forbid Domain/path overrides on __Host-.
func SessionCookieName(opts CookieOptions) string {
	if opts.Insecure {
		return DevSessionCookie
	}
	return SessionCookie
}

func LoginCookieName(opts CookieOptions) string {
	if opts.Insecure {
		return DevLoginCookie
	}
	return LoginCookie
}

func SetSessionCookie(w http.ResponseWriter, opts CookieOptions, token string, ttl time.Duration) {
	clearLegacySessionCookies(w, opts)
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName(opts), Value: token, Path: "/",
		Expires: time.Now().Add(ttl), MaxAge: int(ttl.Seconds()),
		HttpOnly: true, Secure: !opts.Insecure, SameSite: http.SameSiteLaxMode,
	})
}

func ClearSessionCookie(w http.ResponseWriter, opts CookieOptions) {
	expireCookie(w, SessionCookieName(opts), "/", "", !opts.Insecure)
	clearLegacySessionCookies(w, opts)
}

func ReadSessionCookie(r *http.Request, opts CookieOptions) string {
	return readCookie(r, SessionCookieName(opts))
}

func readCookie(r *http.Request, name string) string {
	value, found := "", false
	for _, cookie := range r.Cookies() {
		if cookie.Name != name {
			continue
		}
		if found {
			return ""
		} // Fail closed rather than select an ambiguous cookie.
		value, found = cookie.Value, true
	}
	return value
}

func clearLegacySessionCookies(w http.ResponseWriter, opts CookieOptions) {
	// Both paths were previously issued. Never accept an older root session
	// after logging out of the newer, more-specific cookie.
	for _, path := range []string{"/", "/web/v1"} {
		expireCookie(w, legacySessionCookie, path, "", !opts.Insecure)
		if opts.Domain != "" {
			expireCookie(w, legacySessionCookie, path, opts.Domain, !opts.Insecure)
		}
	}
	clearLegacyLoginCookies(w, opts)
}

func clearLegacyLoginCookies(w http.ResponseWriter, opts CookieOptions) {
	expireCookie(w, legacyLoginCookie, "/", "", !opts.Insecure)
	if opts.Domain != "" {
		expireCookie(w, legacyLoginCookie, "/", opts.Domain, !opts.Insecure)
	}
}

func expireCookie(w http.ResponseWriter, name, path, domain string, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Path: path, Domain: domain, Value: "", MaxAge: -1,
		Expires: time.Unix(1, 0), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}
