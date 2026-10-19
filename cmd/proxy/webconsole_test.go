package main

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/config"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

func TestKeyOnlyPortalStartsWithoutOIDC(t *testing.T) {
	cfg := &config.Config{Web: config.WebConfig{Enabled: true, APIKeyLogin: true, BaseURL: "https://CONSOLE.EXAMPLE.COM:443/"}}
	cfg.Secrets.CredentialEncryptionKey = strings.Repeat("x", 32)
	// Stores are constructed here but do not query until a request arrives.
	console, err := setupWebConsole(context.Background(), cfg, webConsoleInputs{Pool: &pgstore.Pool{}}, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer console.Close()
	if console.deps.Config.BaseURL != "https://console.example.com" {
		t.Fatal("portal did not use the canonical configured origin")
	}
	if console.deps.OIDC != nil || console.deps.KeySessions == nil || console.Factory == nil {
		t.Fatal("key-only portal requires OIDC or is missing key auth")
	}
}

func TestKeyPortalRequiresEncryptionKey(t *testing.T) {
	cfg := &config.Config{Web: config.WebConfig{Enabled: true, APIKeyLogin: true, BaseURL: "https://console.example.com"}}
	if _, err := setupWebConsole(context.Background(), cfg, webConsoleInputs{Pool: &pgstore.Pool{}}, zerolog.Nop()); err == nil {
		t.Fatal("key-cookie login started without encryption")
	}
}
