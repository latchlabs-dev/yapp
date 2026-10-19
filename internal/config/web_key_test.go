package config_test

import (
	"testing"

	"github.com/earlgrey/piproxy/internal/config"
)

func TestWebKeyConfiguration(t *testing.T) {
	base := config.WebConfig{Enabled: true, APIKeyLogin: true, BaseURL: "https://console.example.com"}
	if !base.Ready() {
		t.Fatal("key-only login requires neither OIDC nor domain mapping")
	}
	for _, tc := range []struct {
		name string
		edit func(*config.WebConfig)
		want bool
	}{
		{"no origin", func(w *config.WebConfig) { w.BaseURL = "" }, false},
		{"plain HTTP", func(w *config.WebConfig) { w.BaseURL = "http://console.example.com" }, false},
		{"no login method", func(w *config.WebConfig) { w.APIKeyLogin = false }, false},
		{"partial OIDC", func(w *config.WebConfig) { w.OIDCIssuer = "https://auth.example.com" }, false},
		{"partial OIDC secret", func(w *config.WebConfig) { w.OIDCClientSecret = "secret" }, false},
		{"shared cookie domain", func(w *config.WebConfig) { w.CookieDomain = ".example.com" }, false},
		{"origin user info", func(w *config.WebConfig) { w.BaseURL = "https://user:password@console.example.com" }, false},
		{"origin query", func(w *config.WebConfig) { w.BaseURL = "https://console.example.com?x=y" }, false},
		{"origin path", func(w *config.WebConfig) { w.BaseURL = "https://console.example.com/portal" }, false},
		{"local HTTP development", func(w *config.WebConfig) { w.BaseURL = "http://localhost:3335"; w.CookieInsecure = true }, true},
		{"tailnet HTTP is not development", func(w *config.WebConfig) { w.BaseURL = "http://100.64.0.10:3335"; w.CookieInsecure = true }, false},
		{"trailing slash", func(w *config.WebConfig) { w.BaseURL += "/" }, true},
		{"canonicalizable origin", func(w *config.WebConfig) { w.BaseURL = "https://CONSOLE.EXAMPLE.COM:443/" }, true},
		{"invalid port", func(w *config.WebConfig) { w.BaseURL = "https://console.example.com:65536" }, false},
		{"shared OIDC cookie domain", func(w *config.WebConfig) {
			w.APIKeyLogin = false
			w.OIDCIssuer = "https://idp.test"
			w.OIDCClientID = "client"
			w.OIDCRedirectURL = "https://console.example.com/web/v1/auth/callback"
			w.CookieDomain = ".example.com"
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.edit(&cfg)
			if cfg.Ready() != tc.want {
				t.Errorf("Ready = %v, want %v", cfg.Ready(), tc.want)
			}
		})
	}
	t.Setenv("PROXY_WEB_API_KEY_LOGIN", "false")
	cfg, err := config.Load()
	if err != nil || cfg.Web.APIKeyLogin {
		t.Fatalf("key login toggle ignored: %v", err)
	}
	t.Setenv("PROXY_WEB_API_KEY_LOGIN", "true")
	cfg, err = config.Load()
	if err != nil || !cfg.Web.APIKeyLogin {
		t.Fatalf("key login toggle ignored: %v", err)
	}
}
