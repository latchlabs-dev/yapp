package modelaccess

import "testing"

func TestConfigAllowsBlacklist(t *testing.T) {
	cfg := &Config{Mode: ModeBlacklist, ModelIDs: []string{"corp-glm-5.2"}}
	if cfg.Allows("corp-glm-5.2") {
		t.Fatal("blacklist allowed exact public model")
	}
	if cfg.Allows("glm-5.2") {
		t.Fatal("blacklist allowed equivalent upstream model")
	}
	if !cfg.Allows("corp-other") {
		t.Fatal("blacklist denied unrelated model")
	}
}

func TestConfigAllowsWhitelist(t *testing.T) {
	cfg := &Config{Mode: ModeWhitelist, ModelIDs: []string{"corp-deepseek-v4-flash"}}
	if !cfg.Allows("deepseek-v4-flash") {
		t.Fatal("whitelist denied equivalent upstream model")
	}
	if cfg.Allows("corp-deepseek-v4-pro") {
		t.Fatal("whitelist allowed unlisted model")
	}
}

func TestDefaultConfigAllowsAll(t *testing.T) {
	if !DefaultConfig("t1").Allows("anything") {
		t.Fatal("default config should allow all models")
	}
}

func TestAllowsSyntheticCannotBypassTargetBlacklist(t *testing.T) {
	cfg := &Config{Mode: ModeBlacklist, ModelIDs: []string{"glm-5.3-flash"}}
	if cfg.AllowsSynthetic("standard-flash", "glm-5.3-flash") {
		t.Fatal("synthetic name bypassed target blacklist")
	}
}

func TestAllowsSyntheticCanBeWhitelistedByStableName(t *testing.T) {
	cfg := &Config{Mode: ModeWhitelist, ModelIDs: []string{"standard-flash"}}
	if !cfg.AllowsSynthetic("standard-flash", "glm-5.3-flash") {
		t.Fatal("explicitly whitelisted synthetic name was denied")
	}
}
