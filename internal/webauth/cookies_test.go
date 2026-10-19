package webauth

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestCookieMigrationAndLogoutWithJar(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		t.Run(map[bool]string{false: "production HTTPS", true: "loopback development"}[insecure], func(t *testing.T) {
			opts := CookieOptions{Insecure: insecure}
			origin := "https://console.example.test"
			if insecure {
				origin = "http://localhost:3335"
			}
			u, _ := url.Parse(origin + "/web/v1/me")
			jar, _ := cookiejar.New(nil)
			jar.SetCookies(u, []*http.Cookie{
				{Name: legacySessionCookie, Value: "old-root", Path: "/"},
				{Name: legacySessionCookie, Value: "old-subpath", Path: "/web/v1"},
				{Name: legacyLoginCookie, Value: "old-login", Path: "/"},
			})
			request := func() *http.Request {
				r := httptest.NewRequest("GET", u.String(), nil)
				for _, c := range jar.Cookies(u) {
					r.AddCookie(c)
				}
				return r
			}
			if ReadSessionCookie(request(), opts) != "" {
				t.Fatal("legacy cookie unexpectedly authenticates")
			}
			rec := httptest.NewRecorder()
			SetSessionCookie(rec, opts, "new-session", time.Hour)
			jar.SetCookies(u, rec.Result().Cookies())
			if got := ReadSessionCookie(request(), opts); got != "new-session" {
				t.Fatalf("new session: %q", got)
			}
			for _, c := range jar.Cookies(u) {
				if c.Name == legacySessionCookie || c.Name == legacyLoginCookie {
					t.Fatal("legacy tuple survived login migration")
				}
			}
			rec = httptest.NewRecorder()
			ClearSessionCookie(rec, opts)
			jar.SetCookies(u, rec.Result().Cookies())
			if got := ReadSessionCookie(request(), opts); got != "" || len(jar.Cookies(u)) != 0 {
				t.Fatal("session restored after logout")
			}
		})
	}
}

func TestProductionCookieNeverFallsBackToInjectableNames(t *testing.T) {
	r := httptest.NewRequest("GET", "https://console.example.test/web/v1/me", nil)
	r.AddCookie(&http.Cookie{Name: legacySessionCookie, Value: "attacker-root"})
	r.AddCookie(&http.Cookie{Name: DevSessionCookie, Value: "attacker-dev"})
	if ReadSessionCookie(r, CookieOptions{}) != "" {
		t.Fatal("production accepted an unprefixed cookie")
	}
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "legitimate"})
	if got := ReadSessionCookie(r, CookieOptions{}); got != "legitimate" {
		t.Fatal("legacy collision shadowed protected cookie")
	}
	r.AddCookie(&http.Cookie{Name: SessionCookie, Value: "ambiguous"})
	if ReadSessionCookie(r, CookieOptions{}) != "" {
		t.Fatal("ambiguous cookies did not fail closed")
	}
}

func TestHostPrefixAndExplicitDevelopmentCookieNames(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		opts := CookieOptions{Insecure: insecure, Domain: ".example.test"}
		rec := httptest.NewRecorder()
		SetSessionCookie(rec, opts, "sealed", time.Hour)
		cookies := rec.Result().Cookies()
		c := cookies[len(cookies)-1]
		if c.Name != SessionCookieName(opts) || c.Path != "/" || c.Domain != "" || c.Secure == insecure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
			t.Fatalf("unsafe cookie: %+v", c)
		}
		if insecure && (c.Name == SessionCookie || LoginCookieName(opts) == LoginCookie) {
			t.Fatal("insecure mode used a reserved __Host- name")
		}
	}
}

func TestOIDCLoginCookieUsesHostPrefix(t *testing.T) {
	// OAuth URL construction needs no external issuer here. This exercises the
	// actual login-cookie writer, not just the name helper.
	p := &Provider{}
	p.oauth.Endpoint.AuthURL = "https://idp.example.test/authorize"
	rec := httptest.NewRecorder()
	if _, err := p.StartLogin(rec, "/"); err != nil {
		t.Fatal(err)
	}
	cookies := rec.Result().Cookies()
	c := cookies[len(cookies)-1]
	if c.Name != LoginCookie || c.Path != "/" || c.Domain != "" || !c.Secure || !c.HttpOnly {
		t.Fatalf("unsafe login cookie: %+v", c)
	}
}
