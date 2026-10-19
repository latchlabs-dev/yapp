package config_test

import (
	"testing"

	"github.com/earlgrey/piproxy/internal/config"
)

func TestModelGuardsDefault(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if len(cfg.ModelGuards) != 0 {
		t.Fatal("default guards must not impose deployment policy")
	}
}

func TestModelGuardsEnvOverride(t *testing.T) {
	t.Setenv("PROXY_MODEL_GUARDS", `[{"model":"other-model","message":"use something else"}]`)
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.ModelGuards.Check("deepseek-v4-pro", "") != nil {
		t.Error("env override must replace the default guards")
	}
	if v := cfg.ModelGuards.Check("other-model", ""); v == nil || v.Message != "use something else" {
		t.Error("env guard rule must apply")
	}
}

func TestModelGuardsEnvEmptyArrayDisables(t *testing.T) {
	t.Setenv("PROXY_MODEL_GUARDS", "[]")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.ModelGuards.Check("deepseek-v4-pro", "") != nil {
		t.Error("PROXY_MODEL_GUARDS=[] must disable all guards")
	}
}

func TestModelGuardsEnvInvalidFailsFast(t *testing.T) {
	t.Setenv("PROXY_MODEL_GUARDS", `{"model":`)
	if _, err := config.Load(); err == nil {
		t.Error("invalid PROXY_MODEL_GUARDS must fail config load")
	}
}
