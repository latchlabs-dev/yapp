package config_test

import (
	"os"
	"testing"
	"time"

	"github.com/earlgrey/piproxy/internal/config"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Server.PublicAddr != ":8080" {
		t.Errorf("expected :8080, got %q", cfg.Server.PublicAddr)
	}
	if cfg.Server.AdminAddr != ":8081" {
		t.Errorf("expected :8081, got %q", cfg.Server.AdminAddr)
	}
	if cfg.Server.MetricsAddr != ":9090" {
		t.Errorf("expected :9090, got %q", cfg.Server.MetricsAddr)
	}
	if cfg.Auth.KeyPrefix != "cpk_live_" {
		t.Errorf("expected 'cpk_live_', got %q", cfg.Auth.KeyPrefix)
	}
	if cfg.Secrets.Provider != "env" {
		t.Errorf("expected 'env', got %q", cfg.Secrets.Provider)
	}
	if cfg.Metrics.Enabled != true {
		t.Errorf("expected metrics enabled, got false")
	}
	if cfg.Memory.InjectionEnabled {
		t.Error("expected project-memory injection disabled by default")
	}
	if cfg.Memory.DistillerModel != "deepseek-v4-flash" {
		t.Errorf("expected default distiller model deepseek-v4-flash, got %q", cfg.Memory.DistillerModel)
	}
	if cfg.Memory.DistillationQueueName != "memory_distillation" {
		t.Errorf("unexpected distillation queue %q", cfg.Memory.DistillationQueueName)
	}
	if cfg.Memory.DistillationNightlyUTC != "23:00" {
		t.Errorf("unexpected nightly distillation time %q", cfg.Memory.DistillationNightlyUTC)
	}
	if cfg.Memory.DistillerIdle != 15*time.Minute {
		t.Errorf("unexpected distillation idle delay %s", cfg.Memory.DistillerIdle)
	}
	if cfg.Memory.CompactionMaxTokens != 1200 {
		t.Errorf("unexpected compaction hard cap %d", cfg.Memory.CompactionMaxTokens)
	}
}

func TestCodexQuotaDisabledIsOptIn(t *testing.T) {
	for _, value := range []string{"", "true", "false"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("PROXY_QUOTA_CODEX_DISABLED", value)
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Quota.CodexDisabled != (value == "true") {
				t.Fatalf("CodexDisabled = %v for %q", cfg.Quota.CodexDisabled, value)
			}
		})
	}
}

func TestInsightGenerationFlagSupportsLegacyFallback(t *testing.T) {
	t.Setenv("PROXY_MEMORY_DISTILLER_ENABLED", "true")
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if !cfg.Memory.InsightGenerationEnabled {
		t.Fatal("legacy distiller flag did not enable insight generation")
	}

	t.Setenv("PROXY_INSIGHTS_ENABLED", "false")
	cfg, err = config.Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}
	if cfg.Memory.InsightGenerationEnabled {
		t.Fatal("preferred insights flag must override the legacy flag")
	}
}

func TestLoadFromEnv(t *testing.T) {
	os.Setenv("PROXY_SERVER_PUBLIC_ADDR", ":9999")
	os.Setenv("PROXY_AUTH_KEY_PREFIX", "test_")
	os.Setenv("PROXY_METRICS_ENABLED", "false")
	os.Setenv("PROXY_LOGGING_LEVEL", "debug")
	os.Setenv("PROXY_SERVER_MAX_BODY_BYTES", "1048576")
	os.Setenv("PROXY_HOOKS_EVENTS_FILE", "/tmp/piproxy-test-hooks.jsonl")
	defer func() {
		os.Unsetenv("PROXY_SERVER_PUBLIC_ADDR")
		os.Unsetenv("PROXY_AUTH_KEY_PREFIX")
		os.Unsetenv("PROXY_METRICS_ENABLED")
		os.Unsetenv("PROXY_LOGGING_LEVEL")
		os.Unsetenv("PROXY_SERVER_MAX_BODY_BYTES")
		os.Unsetenv("PROXY_HOOKS_EVENTS_FILE")
	}()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Server.PublicAddr != ":9999" {
		t.Errorf("expected :9999, got %q", cfg.Server.PublicAddr)
	}
	if cfg.Auth.KeyPrefix != "test_" {
		t.Errorf("expected 'test_', got %q", cfg.Auth.KeyPrefix)
	}
	if cfg.Metrics.Enabled != false {
		t.Errorf("expected metrics disabled, got enabled")
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("expected 'debug', got %q", cfg.Logging.Level)
	}
	if cfg.Server.MaxBodyBytes != 1048576 {
		t.Errorf("expected 1048576, got %d", cfg.Server.MaxBodyBytes)
	}
	if cfg.Hooks.EventsFile != "/tmp/piproxy-test-hooks.jsonl" {
		t.Errorf("unexpected hook events file %q", cfg.Hooks.EventsFile)
	}
}

func TestBuildInfo(t *testing.T) {
	info := config.BuildInfo()
	if info["version"] == "" {
		t.Error("expected version to be non-empty")
	}
	if info["go_version"] == "" {
		t.Error("expected go_version to be non-empty")
	}
	if info["go_os"] == "" {
		t.Error("expected go_os to be non-empty")
	}
}

func TestLoadPostgresDefaults(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	expectedDSN := "postgres://proxy:proxy@localhost:5432/proxy?sslmode=disable"
	if cfg.Postgres.DSN != expectedDSN {
		t.Errorf("expected %q, got %q", expectedDSN, cfg.Postgres.DSN)
	}
	if cfg.Postgres.MaxOpenConns != 25 {
		t.Errorf("expected 25, got %d", cfg.Postgres.MaxOpenConns)
	}
}

func TestLoadRedisDefaults(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() failed: %v", err)
	}

	if cfg.Redis.Address != "localhost:6379" {
		t.Errorf("expected 'localhost:6379', got %q", cfg.Redis.Address)
	}
	if cfg.Redis.DB != 0 {
		t.Errorf("expected 0, got %d", cfg.Redis.DB)
	}
}

func TestValidateProductionSafetyPostgresSSLMode(t *testing.T) {
	tests := []struct {
		name    string
		cfg     config.Config
		wantErr bool
	}{
		{
			name: "remote disable rejected for public proxy",
			cfg: config.Config{
				Server:   config.ServerConfig{PublicBaseURL: "https://proxy.example.com"},
				Postgres: config.PostgresConfig{DSN: "postgres://proxy:proxy@db.example.com:5432/proxy?sslmode=disable"},
			},
			wantErr: true,
		},
		{
			name: "missing sslmode rejected for public proxy remote postgres",
			cfg: config.Config{
				Server:   config.ServerConfig{PublicBaseURL: "https://proxy.example.com"},
				Postgres: config.PostgresConfig{DSN: "postgres://proxy:proxy@db.example.com:5432/proxy"},
			},
			wantErr: true,
		},
		{
			name: "local disable allowed",
			cfg: config.Config{
				Server:   config.ServerConfig{PublicBaseURL: "https://proxy.example.com"},
				Postgres: config.PostgresConfig{DSN: "postgres://proxy:proxy@localhost:5433/proxy?sslmode=disable"},
			},
		},
		{
			name: "tailscale disable allowed",
			cfg: config.Config{
				Server:   config.ServerConfig{PublicBaseURL: "https://proxy.example.com"},
				Postgres: config.PostgresConfig{DSN: "postgres://proxy:proxy@100.64.0.11:5432/proxy?sslmode=disable"},
			},
		},
		{
			name: "remote require allowed",
			cfg: config.Config{
				Server:   config.ServerConfig{PublicBaseURL: "https://proxy.example.com"},
				Postgres: config.PostgresConfig{DSN: "postgres://proxy:proxy@db.example.com:5432/proxy?sslmode=require"},
			},
		},
		{
			name: "schemeless public base url still rejects remote disable",
			cfg: config.Config{
				Server:   config.ServerConfig{PublicBaseURL: "proxy.example.com"},
				Postgres: config.PostgresConfig{DSN: "postgres://proxy:proxy@db.example.com:5432/proxy?sslmode=disable"},
			},
			wantErr: true,
		},
		{
			name: "dev remote disable allowed when proxy not public",
			cfg: config.Config{
				Postgres: config.PostgresConfig{DSN: "postgres://proxy:proxy@db.example.com:5432/proxy?sslmode=disable"},
			},
		},
		{
			name: "rate-limited public bind rejects remote disable",
			cfg: config.Config{
				Server:    config.ServerConfig{PublicAddr: ":8080"},
				RateLimit: config.RateLimitConfig{RequestsPerMinute: 120},
				Postgres:  config.PostgresConfig{DSN: "host=db.example.com port=5432 dbname=proxy sslmode=disable"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.ValidateProductionSafety()
			if tt.wantErr && err == nil {
				t.Fatal("expected validation error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

// The console must fail closed on partial configuration. Starting with login
// broken looks like an outage to every user, so setupWebConsole treats a false
// Ready() as fatal rather than degrading.
func TestWebConfigReadyRequiresTheFullOIDCTriple(t *testing.T) {
	full := config.WebConfig{
		Enabled:         true,
		BaseURL:         "https://console.example.com",
		OIDCIssuer:      "https://auth.example.com",
		OIDCClientID:    "console",
		OIDCRedirectURL: "https://console.example.com/web/v1/auth/callback",
	}
	if !full.Ready() {
		t.Fatal("a fully configured console should be ready")
	}

	cases := map[string]config.WebConfig{
		"disabled":     {OIDCIssuer: "i", OIDCClientID: "c", OIDCRedirectURL: "r"},
		"no issuer":    {Enabled: true, OIDCClientID: "c", OIDCRedirectURL: "r"},
		"no client id": {Enabled: true, OIDCIssuer: "i", OIDCRedirectURL: "r"},
		"no redirect":  {Enabled: true, OIDCIssuer: "i", OIDCClientID: "c"},
	}
	for name, cfg := range cases {
		if cfg.Ready() {
			t.Errorf("%s: Ready() = true, want false", name)
		}
	}
}

// The console still runs without a bucket; only uploads are refused. That keeps
// a missing bucket from taking chat down.
func TestWebConfigAttachmentsAreIndependentlyOptional(t *testing.T) {
	cfg := config.WebConfig{Enabled: true, OIDCIssuer: "i", OIDCClientID: "c", OIDCRedirectURL: "r"}
	if cfg.AttachmentsReady() {
		t.Error("AttachmentsReady() = true with no bucket configured")
	}
	cfg.AttachmentsBucket = "piproxy-attachments"
	if !cfg.AttachmentsReady() {
		t.Error("AttachmentsReady() = false with a bucket configured")
	}
}

// Both new subsystems default off: the console because it needs an IdP, and
// hook subscriptions because blocking hooks add latency to existing CLI traffic.
func TestWebAndHookSubsystemsDefaultOff(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Web.Enabled {
		t.Error("the web console must default to disabled")
	}
	if cfg.Hooks.SubscriptionsEnabled {
		t.Error("hook subscriptions must default to disabled")
	}
	if cfg.Hooks.AllowPrivateEgress {
		t.Error("private hook egress must default to disabled: it is an SSRF primitive")
	}
	if cfg.Hooks.BlockingBudget <= 0 {
		t.Error("a blocking hook budget of zero would leave hook latency unbounded")
	}
	if cfg.Web.SessionTTL <= 0 || cfg.Web.AttachmentMaxBytes <= 0 {
		t.Errorf("web defaults are not sane: ttl=%v maxBytes=%d", cfg.Web.SessionTTL, cfg.Web.AttachmentMaxBytes)
	}
}
