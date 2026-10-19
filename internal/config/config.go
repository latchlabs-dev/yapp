// Package config provides configuration loading for the proxy service.
package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/earlgrey/piproxy/internal/modelguard"
	"github.com/earlgrey/piproxy/internal/weborigin"
)

// Build-time version information, injected via ldflags.
var (
	Version   = "dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)

// ServerConfig holds HTTP server configuration.
type ServerConfig struct {
	PublicAddr      string        `yaml:"public_addr" env:"PROXY_SERVER_PUBLIC_ADDR" default:":8080"`
	AdminAddr       string        `yaml:"admin_addr" env:"PROXY_SERVER_ADMIN_ADDR" default:":8081"`
	MetricsAddr     string        `yaml:"metrics_addr" env:"PROXY_SERVER_METRICS_ADDR" default:":9090"`
	MaxBodyBytes    int64         `yaml:"max_body_bytes" env:"PROXY_SERVER_MAX_BODY_BYTES" default:"20971520"`
	RequestTimeout  time.Duration `yaml:"request_timeout" env:"PROXY_SERVER_REQUEST_TIMEOUT" default:"10m"`
	ReadTimeout     time.Duration `yaml:"read_timeout" env:"PROXY_SERVER_READ_TIMEOUT" default:"30s"`
	WriteTimeout    time.Duration `yaml:"write_timeout" env:"PROXY_SERVER_WRITE_TIMEOUT" default:"5m"`
	IdleTimeout     time.Duration `yaml:"idle_timeout" env:"PROXY_SERVER_IDLE_TIMEOUT" default:"120s"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" env:"PROXY_SERVER_SHUTDOWN_TIMEOUT" default:"30s"`
	PublicBaseURL   string        `yaml:"public_base_url" env:"PROXY_PUBLIC_BASE_URL" default:""`
}

// AuthConfig holds authentication configuration.
type AuthConfig struct {
	KeyPrefix     string `yaml:"key_prefix" env:"PROXY_AUTH_KEY_PREFIX" default:"cpk_live_"`
	HashPepperRef string `yaml:"hash_pepper_secret_ref" env:"PROXY_AUTH_HASH_PEPPER_REF" default:""`
}

// PostgresConfig holds Postgres connection configuration.
type PostgresConfig struct {
	DSN          string `yaml:"dsn" env:"PROXY_POSTGRES_DSN" default:"postgres://proxy:proxy@localhost:5432/proxy?sslmode=disable"`
	MaxOpenConns int    `yaml:"max_open_conns" env:"PROXY_POSTGRES_MAX_OPEN_CONNS" default:"25"`
	MaxIdleConns int    `yaml:"max_idle_conns" env:"PROXY_POSTGRES_MAX_IDLE_CONNS" default:"0"`
}

// RedisConfig holds Redis connection configuration.
type RedisConfig struct {
	Address  string `yaml:"address" env:"PROXY_REDIS_ADDRESS" default:"localhost:6379"`
	Password string `yaml:"password" env:"PROXY_REDIS_PASSWORD" default:""`
	DB       int    `yaml:"db" env:"PROXY_REDIS_DB" default:"0"`
	TLS      bool   `yaml:"tls" env:"PROXY_REDIS_TLS" default:"false"`
}

// SecretsConfig holds secret store configuration.
type SecretsConfig struct {
	Provider                string `yaml:"provider" env:"PROXY_SECRETS_PROVIDER" default:"env"`
	Address                 string `yaml:"address" env:"PROXY_SECRETS_ADDRESS" default:""`
	Token                   string `yaml:"-" env:"PROXY_SECRETS_TOKEN" default:""`
	Namespace               string `yaml:"namespace" env:"PROXY_SECRETS_NAMESPACE" default:""`
	EnvPrefix               string `yaml:"env_prefix" env:"PROXY_SECRETS_ENV_PREFIX" default:"PROXY_SECRET_"`
	CredentialEncryptionKey string `yaml:"credential_encryption_key" env:"PROXY_CREDENTIAL_ENCRYPTION_KEY" default:""`
}

// MetricsConfig holds Prometheus metrics configuration.
type MetricsConfig struct {
	Enabled bool `yaml:"enabled" env:"PROXY_METRICS_ENABLED" default:"true"`
}

// LoggingConfig holds logging configuration.
type LoggingConfig struct {
	Level     string `yaml:"level" env:"PROXY_LOGGING_LEVEL" default:"info"`
	Format    string `yaml:"format" env:"PROXY_LOGGING_FORMAT" default:"json"`
	LogBodies bool   `yaml:"log_bodies" env:"PROXY_LOGGING_LOG_BODIES" default:"false"`
}

// QuotaConfig holds quota system configuration.
type QuotaConfig struct {
	ReservationTTL time.Duration `yaml:"reservation_ttl" env:"PROXY_QUOTA_RESERVATION_TTL" default:"15m"`
	CodexDisabled  bool          `yaml:"codex_disabled" env:"PROXY_QUOTA_CODEX_DISABLED" default:"false"`
}

// CaptureConfig controls async raw conversation payload capture.
type CaptureConfig struct {
	Enabled       bool   `yaml:"enabled" env:"PROXY_CAPTURE_ENABLED" default:"true"`
	Backend       string `yaml:"backend" env:"PROXY_CAPTURE_BACKEND" default:"postgres"`
	RetentionDays int    `yaml:"retention_days" env:"PROXY_CAPTURE_RETENTION_DAYS" default:"30"`
	QueueSize     int    `yaml:"queue_size" env:"PROXY_CAPTURE_QUEUE_SIZE" default:"2000"`
	Workers       int    `yaml:"workers" env:"PROXY_CAPTURE_WORKERS" default:"4"`
	// MaxPayloadBytes caps stored capture bodies. 0 (the default) disables
	// truncation entirely so sessions stay replayable: request bodies are
	// already bounded by the body-size middleware, buffered responses by
	// ReadUpstreamBody, and SSE transcripts by the stream-capture ceiling.
	// When set (>0), bodies over the limit are cut and a capture_gaps marker
	// records the loss.
	MaxPayloadBytes         int64 `yaml:"max_payload_bytes" env:"PROXY_CAPTURE_MAX_PAYLOAD_BYTES" default:"0"`
	Strict                  bool  `yaml:"strict" env:"PROXY_CAPTURE_STRICT" default:"false"`
	SessionLogRetentionDays int   `yaml:"session_log_retention_days" env:"PROXY_SESSION_LOG_RETENTION_DAYS" default:"30"`
}

// Session archive object backends and raw-body placements.
const (
	SessionObjectsPostgres = "postgres"
	SessionObjectsGCS      = "gcs"

	SessionRawBodiesPostgres = "postgres"
	SessionRawBodiesObjects  = "objects"
	SessionRawBodiesBoth     = "both"
)

// SessionArchiveConfig controls where captured session bodies live and
// whether transcripts are built at capture time. The proxy and admin processes
// must share these values: admin reads what the proxy writes.
type SessionArchiveConfig struct {
	Enabled        bool   `yaml:"enabled" env:"PROXY_SESSION_ARCHIVE_ENABLED" default:"true"`
	ObjectsBackend string `yaml:"objects_backend" env:"PROXY_SESSION_OBJECTS_BACKEND" default:"postgres"`
	ObjectsBucket  string `yaml:"objects_bucket" env:"PROXY_SESSION_OBJECTS_BUCKET" default:""`
	ObjectsPrefix  string `yaml:"objects_prefix" env:"PROXY_SESSION_OBJECTS_PREFIX" default:"piproxy"`
	// RawBodies places raw request/response bodies: in Postgres bytea
	// (legacy), in object storage (kept forever, no retention), or both during
	// a migration safety window. Defaults to objects when the backend is GCS.
	RawBodies     string        `yaml:"raw_bodies" env:"PROXY_SESSION_RAW_BODIES" default:""`
	MergeSegments int           `yaml:"merge_segments" env:"PROXY_SESSION_ARCHIVE_MERGE_SEGMENTS" default:"32"`
	QueryURL      string        `yaml:"query_url" env:"PROXY_SESSION_QUERY_URL" default:""`
	QueryToken    string        `yaml:"query_token" env:"PROXY_SESSION_QUERY_TOKEN" default:""`
	QueryTimeout  time.Duration `yaml:"query_timeout" env:"PROXY_SESSION_QUERY_TIMEOUT" default:"750ms"`
}

// Validate rejects inconsistent archive settings at startup rather than
// letting captures fail one by one.
func (s SessionArchiveConfig) Validate() error {
	switch s.ObjectsBackend {
	case SessionObjectsPostgres:
	case SessionObjectsGCS:
		if strings.TrimSpace(s.ObjectsBucket) == "" {
			return fmt.Errorf("PROXY_SESSION_OBJECTS_BUCKET is required when PROXY_SESSION_OBJECTS_BACKEND=gcs")
		}
	default:
		return fmt.Errorf("unsupported PROXY_SESSION_OBJECTS_BACKEND %q (want postgres or gcs)", s.ObjectsBackend)
	}
	switch s.RawBodies {
	case SessionRawBodiesPostgres:
	case SessionRawBodiesObjects, SessionRawBodiesBoth:
		if s.ObjectsBackend != SessionObjectsGCS {
			return fmt.Errorf("PROXY_SESSION_RAW_BODIES=%s requires PROXY_SESSION_OBJECTS_BACKEND=gcs", s.RawBodies)
		}
	default:
		return fmt.Errorf("unsupported PROXY_SESSION_RAW_BODIES %q (want postgres, objects or both)", s.RawBodies)
	}
	if s.MergeSegments < 2 {
		return fmt.Errorf("PROXY_SESSION_ARCHIVE_MERGE_SEGMENTS must be at least 2, got %d", s.MergeSegments)
	}
	if s.QueryURL != "" {
		if len(s.QueryToken) < 32 || s.QueryTimeout <= 0 {
			return fmt.Errorf("session query requires a 32-character PROXY_SESSION_QUERY_TOKEN and a positive timeout")
		}
		if _, err := sessionQueryURL(s.QueryURL); err != nil {
			return err
		}
	}
	return nil
}

// RawBodiesInObjects reports whether raw capture bodies are written to object
// storage.
func (s SessionArchiveConfig) RawBodiesInObjects() bool {
	return s.RawBodies == SessionRawBodiesObjects || s.RawBodies == SessionRawBodiesBoth
}

// RawBodiesInPostgres reports whether raw capture bodies are written to the
// conversation_payloads bytea columns.
func (s SessionArchiveConfig) RawBodiesInPostgres() bool {
	return s.RawBodies == SessionRawBodiesPostgres || s.RawBodies == SessionRawBodiesBoth
}

// UsageConfig controls the async usage-event recorder. Usage events are the
// canonical per-request accounting/abuse record, so the queue and worker pool
// are tunable rather than hardcoded.
type UsageConfig struct {
	QueueSize int `yaml:"queue_size" env:"PROXY_USAGE_QUEUE_SIZE" default:"4000"`
	Workers   int `yaml:"workers" env:"PROXY_USAGE_WORKERS" default:"4"`
}

// HooksConfig controls metadata-only user/session hook persistence and delivery.
type HooksConfig struct {
	EventsFile    string        `yaml:"events_file" env:"PROXY_HOOKS_EVENTS_FILE" default:""`
	WebhookURL    string        `yaml:"webhook_url" env:"PROXY_HOOKS_WEBHOOK_URL" default:""`
	WebhookSecret string        `yaml:"-" env:"PROXY_HOOKS_WEBHOOK_SECRET" default:""`
	QueueSize     int           `yaml:"queue_size" env:"PROXY_HOOKS_QUEUE_SIZE" default:"1000"`
	Timeout       time.Duration `yaml:"timeout" env:"PROXY_HOOKS_TIMEOUT" default:"1500ms"`

	// SubscriptionsEnabled turns on database-managed hooks. It defaults to off
	// because blocking subscriptions add latency to every matching request,
	// including existing CLI traffic; roll it out per tenant rather than
	// flipping it on for everyone at once.
	SubscriptionsEnabled bool `yaml:"subscriptions_enabled" env:"PROXY_HOOKS_SUBSCRIPTIONS_ENABLED" default:"false"`
	// RefreshInterval controls how quickly a subscription change in the admin
	// UI takes effect.
	RefreshInterval time.Duration `yaml:"refresh_interval" env:"PROXY_HOOKS_REFRESH_INTERVAL" default:"30s"`
	// BlockingBudget bounds the total time all blocking hooks may add to one
	// request. It is a budget across every hook, not per hook.
	BlockingBudget time.Duration `yaml:"blocking_budget" env:"PROXY_HOOKS_BLOCKING_BUDGET" default:"3s"`
	// AllowPrivateEgress permits hook URLs on private networks and over http.
	// Development and the live-test harness only; leaving it on in production
	// turns the hook registry into an SSRF primitive.
	AllowPrivateEgress bool `yaml:"allow_private_egress" env:"PROXY_HOOKS_ALLOW_PRIVATE_EGRESS" default:"false"`
	// DeliveryRetentionDays trims the delivery log, which is diagnostics rather
	// than an audit record.
	DeliveryRetentionDays int `yaml:"delivery_retention_days" env:"PROXY_HOOKS_DELIVERY_RETENTION_DAYS" default:"14"`
}

// ProvidersConfig holds provider registry configuration.
type ProvidersConfig struct {
	RefreshInterval        time.Duration `yaml:"refresh_interval" env:"PROXY_PROVIDERS_REFRESH_INTERVAL" default:"30s"`
	BalanceRefreshInterval time.Duration `yaml:"balance_refresh_interval" env:"PROXY_PROVIDERS_BALANCE_REFRESH_INTERVAL" default:"5m"`
	BalanceRequestTimeout  time.Duration `yaml:"balance_request_timeout" env:"PROXY_PROVIDERS_BALANCE_REQUEST_TIMEOUT" default:"10s"`
}

// UpstreamConfig holds upstream HTTP client configuration.
type UpstreamConfig struct {
	Timeout         time.Duration `yaml:"timeout" env:"PROXY_UPSTREAM_TIMEOUT" default:"10m"`
	MaxConnsPerHost int           `yaml:"max_conns_per_host" env:"PROXY_UPSTREAM_MAX_CONNS_PER_HOST" default:"100"`
	MaxIdleConns    int           `yaml:"max_idle_conns" env:"PROXY_UPSTREAM_MAX_IDLE_CONNS" default:"100"`
}

// DuffelConfig holds the duffel workspace connection. Leaving Root empty
// disables the whole project-memory feature set.
type DuffelConfig struct {
	Root         string `yaml:"root" env:"PROXY_DUFFEL_ROOT" default:""`
	URL          string `yaml:"url" env:"PROXY_DUFFEL_URL" default:""` // deprecated: HTTP Duffel service is no longer used
	Token        string `yaml:"-" env:"PROXY_DUFFEL_TOKEN" default:""` // deprecated
	EnableSearch bool   `yaml:"enable_search" env:"PROXY_DUFFEL_ENABLE_SEARCH" default:"false"`
	Collection   string `yaml:"collection" env:"PROXY_DUFFEL_COLLECTION" default:"piproxy"`
}

// MemoryConfig tunes project-memory context injection, external insight queueing,
// and compaction. Insight service endpoints and matching policies live in
// Postgres and are managed through the admin UI.
type MemoryConfig struct {
	ContextBudgetTokens      int           `yaml:"context_budget_tokens" env:"PROXY_MEMORY_CONTEXT_BUDGET_TOKENS" default:"4000"`
	ContextCacheTTL          time.Duration `yaml:"context_cache_ttl" env:"PROXY_MEMORY_CONTEXT_CACHE_TTL" default:"2m"`
	SearchLimit              int           `yaml:"search_limit" env:"PROXY_MEMORY_SEARCH_LIMIT" default:"5"`
	SearchMinScore           float64       `yaml:"search_min_score" env:"PROXY_MEMORY_SEARCH_MIN_SCORE" default:"0"`
	SearchIntent             string        `yaml:"search_intent" env:"PROXY_MEMORY_SEARCH_INTENT" default:""`
	RecentLessons            int           `yaml:"recent_lessons" env:"PROXY_MEMORY_RECENT_LESSONS" default:"10"`
	MinBundleTokens          int           `yaml:"min_bundle_tokens" env:"PROXY_MEMORY_MIN_BUNDLE_TOKENS" default:"80"`
	ObservationMaxBytes      int           `yaml:"observation_max_bytes" env:"PROXY_MEMORY_OBSERVATION_MAX_BYTES" default:"4096"`
	InjectionEnabled         bool          `yaml:"injection_enabled" env:"PROXY_MEMORY_INJECTION_ENABLED" default:"false"`
	InsightGenerationEnabled bool          `yaml:"insight_generation_enabled" env:"PROXY_INSIGHTS_ENABLED" default:"false"`
	DistillerModel           string        `yaml:"distiller_model" env:"PROXY_MEMORY_DISTILLER_MODEL" default:"deepseek-v4-flash"`
	DistillationQueueName    string        `yaml:"distillation_queue_name" env:"PROXY_MEMORY_DISTILLATION_QUEUE_NAME" default:"memory_distillation"`
	DistillationWorkers      int           `yaml:"distillation_workers" env:"PROXY_MEMORY_DISTILLATION_WORKERS" default:"1"`
	DistillationPoll         time.Duration `yaml:"distillation_poll_interval" env:"PROXY_MEMORY_DISTILLATION_POLL_INTERVAL" default:"30s"`
	DistillationClaim        time.Duration `yaml:"distillation_claim_timeout" env:"PROXY_MEMORY_DISTILLATION_CLAIM_TIMEOUT" default:"30m"`
	DistillationAttempts     int           `yaml:"distillation_max_attempts" env:"PROXY_MEMORY_DISTILLATION_MAX_ATTEMPTS" default:"3"`
	DistillationNightlyUTC   string        `yaml:"distillation_nightly_at_utc" env:"PROXY_MEMORY_DISTILLATION_NIGHTLY_AT_UTC" default:"23:00"`
	DistillerIdle            time.Duration `yaml:"distiller_idle" env:"PROXY_MEMORY_DISTILLER_IDLE" default:"15m"`
	CompactionEnabled        bool          `yaml:"compaction_enabled" env:"PROXY_MEMORY_COMPACTION_ENABLED" default:"true"`
	CompactionInterval       time.Duration `yaml:"compaction_interval" env:"PROXY_MEMORY_COMPACTION_INTERVAL" default:"6h"`
	CompactionThreshold      int           `yaml:"compaction_threshold_tokens" env:"PROXY_MEMORY_COMPACTION_THRESHOLD_TOKENS" default:"8000"`
	CompactionMaxTokens      int           `yaml:"compaction_max_tokens" env:"PROXY_MEMORY_COMPACTION_MAX_TOKENS" default:"1200"`
	CompactorModel           string        `yaml:"compactor_model" env:"PROXY_MEMORY_COMPACTOR_MODEL" default:""`
	CompactionQueue          bool          `yaml:"compaction_queue" env:"PROXY_MEMORY_COMPACTION_QUEUE" default:"true"`
	CompactionQueueName      string        `yaml:"compaction_queue_name" env:"PROXY_MEMORY_COMPACTION_QUEUE_NAME" default:"memory_compaction"`
	CompactionWorkers        int           `yaml:"compaction_workers" env:"PROXY_MEMORY_COMPACTION_WORKERS" default:"1"`
	CompactionPoll           time.Duration `yaml:"compaction_poll_interval" env:"PROXY_MEMORY_COMPACTION_POLL_INTERVAL" default:"30s"`
	CompactionClaim          time.Duration `yaml:"compaction_claim_timeout" env:"PROXY_MEMORY_COMPACTION_CLAIM_TIMEOUT" default:"30m"`
	CompactionPeakUTC        string        `yaml:"compaction_peak_windows_utc" env:"PROXY_MEMORY_COMPACTION_PEAK_WINDOWS_UTC" default:"01:00-04:00,06:00-10:00"`
}

// WebConfig holds the Tailscale-only portal: API-key cookie login, optional
// OIDC, and GCS-backed attachments. The portal stays disabled by default.
type WebConfig struct {
	Enabled            bool          `yaml:"enabled" env:"PROXY_WEB_ENABLED" default:"false"`
	APIKeyLogin        bool          `yaml:"api_key_login" env:"PROXY_WEB_API_KEY_LOGIN" default:"true"`
	BaseURL            string        `yaml:"base_url" env:"PROXY_WEB_BASE_URL" default:""`
	OIDCIssuer         string        `yaml:"oidc_issuer" env:"PROXY_WEB_OIDC_ISSUER" default:""`
	OIDCClientID       string        `yaml:"oidc_client_id" env:"PROXY_WEB_OIDC_CLIENT_ID" default:""`
	OIDCClientSecret   string        `yaml:"-" env:"PROXY_WEB_OIDC_CLIENT_SECRET" default:""`
	OIDCRedirectURL    string        `yaml:"oidc_redirect_url" env:"PROXY_WEB_OIDC_REDIRECT_URL" default:""`
	OIDCProviderName   string        `yaml:"oidc_provider_name" env:"PROXY_WEB_OIDC_PROVIDER_NAME" default:"your work account"`
	OIDCScopes         string        `yaml:"oidc_scopes" env:"PROXY_WEB_OIDC_SCOPES" default:"openid,profile,email"`
	SessionTTL         time.Duration `yaml:"session_ttl" env:"PROXY_WEB_SESSION_TTL" default:"12h"`
	SessionIdleTTL     time.Duration `yaml:"session_idle_ttl" env:"PROXY_WEB_SESSION_IDLE_TTL" default:"2h"`
	CookieDomain       string        `yaml:"cookie_domain" env:"PROXY_WEB_COOKIE_DOMAIN" default:""`
	CookieInsecure     bool          `yaml:"cookie_insecure" env:"PROXY_WEB_COOKIE_INSECURE" default:"false"`
	MaxHistoryMessages int           `yaml:"max_history_messages" env:"PROXY_WEB_MAX_HISTORY_MESSAGES" default:"200"`

	AttachmentsBucket        string        `yaml:"attachments_bucket" env:"PROXY_WEB_ATTACHMENTS_BUCKET" default:""`
	AttachmentsPrefix        string        `yaml:"attachments_prefix" env:"PROXY_WEB_ATTACHMENTS_PREFIX" default:"attachments"`
	AttachmentMaxBytes       int64         `yaml:"attachment_max_bytes" env:"PROXY_WEB_ATTACHMENT_MAX_BYTES" default:"26214400"`
	AttachmentRetention      int           `yaml:"attachment_retention_days" env:"PROXY_WEB_ATTACHMENT_RETENTION_DAYS" default:"90"`
	AttachmentURLTTL         time.Duration `yaml:"attachment_url_ttl" env:"PROXY_WEB_ATTACHMENT_URL_TTL" default:"10m"`
	AttachmentPendingTTL     time.Duration `yaml:"attachment_pending_ttl" env:"PROXY_WEB_ATTACHMENT_PENDING_TTL" default:"1h"`
	AttachmentUserBytes      int64         `yaml:"attachment_user_bytes" env:"PROXY_WEB_ATTACHMENT_USER_BYTES" default:"1073741824"`
	AttachmentTenantBytes    int64         `yaml:"attachment_tenant_bytes" env:"PROXY_WEB_ATTACHMENT_TENANT_BYTES" default:"10737418240"`
	AttachmentPendingPerUser int64         `yaml:"attachment_pending_per_user" env:"PROXY_WEB_ATTACHMENT_PENDING_PER_USER" default:"16"`

	// WriteRequestsPerMinute bounds per-user console writes (messages,
	// conversations, uploads) per process. A user message is persisted before
	// the delegated handler checks quota, so without this a denied turn still
	// adds a row every time. 0 disables the guard.
	WriteRequestsPerMinute int64 `yaml:"write_requests_per_minute" env:"PROXY_WEB_WRITE_REQUESTS_PER_MINUTE" default:"60"`
}

// Ready reports whether the web console has the minimum configuration to serve
// authenticated routes. Callers fail closed on false rather than starting a
// login flow that cannot complete.
func (w WebConfig) Ready() bool {
	if !w.Enabled || (!w.APIKeyLogin && !w.OIDCReady()) || (w.OIDCConfigured() && !w.OIDCReady()) {
		return false
	}
	canonical, err := weborigin.Canonical(w.BaseURL)
	if err != nil || w.CookieDomain != "" {
		return false // All production cookies use __Host-; shared domains are forbidden.
	}
	origin, _ := url.Parse(canonical)
	if !w.CookieInsecure {
		return origin.Scheme == "https"
	}
	// Disabling Secure cookies is exclusively a loopback development option.
	ip := net.ParseIP(origin.Hostname())
	return origin.Scheme == "http" && (origin.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()))
}

func (w WebConfig) OIDCReady() bool {
	return w.OIDCIssuer != "" && w.OIDCClientID != "" && w.OIDCRedirectURL != ""
}

func (w WebConfig) OIDCConfigured() bool {
	return w.OIDCIssuer != "" || w.OIDCClientID != "" || w.OIDCRedirectURL != "" || w.OIDCClientSecret != ""
}

// AttachmentsReady reports whether attachment storage is configured. The
// console still runs without it; uploads are refused.
func (w WebConfig) AttachmentsReady() bool {
	return w.AttachmentsBucket != ""
}

// Config is the top-level configuration for the proxy service.
type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Upstream UpstreamConfig `yaml:"upstream"`
	Auth     AuthConfig     `yaml:"auth"`
	Postgres PostgresConfig `yaml:"postgres"`
	Redis    RedisConfig    `yaml:"redis"`
	Secrets  SecretsConfig  `yaml:"secrets"`
	Metrics  MetricsConfig  `yaml:"metrics"`
	Logging  LoggingConfig  `yaml:"logging"`
	Quota    QuotaConfig    `yaml:"quota"`
	Capture  CaptureConfig  `yaml:"capture"`
	// SessionArchive places session bodies in object storage.
	SessionArchive SessionArchiveConfig `yaml:"session_archive"`
	Usage          UsageConfig          `yaml:"usage"`
	Hooks          HooksConfig          `yaml:"hooks"`
	Providers      ProvidersConfig      `yaml:"providers"`
	RateLimit      RateLimitConfig      `yaml:"rate_limit"`
	Duffel         DuffelConfig         `yaml:"duffel"`
	Memory         MemoryConfig         `yaml:"memory"`
	Web            WebConfig            `yaml:"web"`
	// ModelGuards stop requests for guarded models with a custom error
	// message (PROXY_MODEL_GUARDS; defaults to modelguard.DefaultRules).
	ModelGuards modelguard.Rules `yaml:"-"`
}

// RateLimitConfig holds IP rate limiter configuration.
type RateLimitConfig struct {
	RequestsPerMinute int64 `yaml:"requests_per_minute" env:"PROXY_RATELIMIT_REQUESTS_PER_MINUTE" default:"0"`
}

// Load reads configuration from environment variables with sensible defaults.
// This uses a simple env-based approach for Phase 0; YAML file support can be added later.
func Load() (*Config, error) {
	cfg := &Config{}
	if err := cfg.loadFromEnv(); err != nil {
		return nil, fmt.Errorf("config load: %w", err)
	}
	if err := cfg.SessionArchive.Validate(); err != nil {
		return nil, fmt.Errorf("config load: %w", err)
	}
	return cfg, nil
}

// loadFromEnv populates config from environment variables.
func (c *Config) loadFromEnv() error {
	c.Server.PublicAddr = getEnv("PROXY_SERVER_PUBLIC_ADDR", ":8080")
	c.Server.AdminAddr = getEnv("PROXY_SERVER_ADMIN_ADDR", ":8081")
	c.Server.MetricsAddr = getEnv("PROXY_SERVER_METRICS_ADDR", ":9090")
	c.Server.MaxBodyBytes = getEnvInt64("PROXY_SERVER_MAX_BODY_BYTES", 20*1024*1024)
	c.Server.RequestTimeout = getEnvDuration("PROXY_SERVER_REQUEST_TIMEOUT", 10*time.Minute)
	c.Server.ReadTimeout = getEnvDuration("PROXY_SERVER_READ_TIMEOUT", 30*time.Second)
	c.Server.WriteTimeout = getEnvDuration("PROXY_SERVER_WRITE_TIMEOUT", 5*time.Minute)
	c.Server.IdleTimeout = getEnvDuration("PROXY_SERVER_IDLE_TIMEOUT", 120*time.Second)
	c.Server.ShutdownTimeout = getEnvDuration("PROXY_SERVER_SHUTDOWN_TIMEOUT", 30*time.Second)
	c.Server.PublicBaseURL = getEnv("PROXY_PUBLIC_BASE_URL", "")

	c.Auth.KeyPrefix = getEnv("PROXY_AUTH_KEY_PREFIX", "cpk_live_")
	c.Auth.HashPepperRef = getEnv("PROXY_AUTH_HASH_PEPPER_REF", "")

	c.Postgres.DSN = getEnv("PROXY_POSTGRES_DSN", "postgres://proxy:proxy@localhost:5432/proxy?sslmode=disable")
	c.Postgres.MaxOpenConns = getEnvInt("PROXY_POSTGRES_MAX_OPEN_CONNS", 25)
	c.Postgres.MaxIdleConns = getEnvInt("PROXY_POSTGRES_MAX_IDLE_CONNS", 0)

	c.Redis.Address = getEnv("PROXY_REDIS_ADDRESS", "localhost:6379")
	c.Redis.Password = getEnv("PROXY_REDIS_PASSWORD", "")
	c.Redis.DB = getEnvInt("PROXY_REDIS_DB", 0)
	c.Redis.TLS = getEnvBool("PROXY_REDIS_TLS", false)

	c.Secrets.Provider = getEnv("PROXY_SECRETS_PROVIDER", "env")
	c.Secrets.Address = getEnv("PROXY_SECRETS_ADDRESS", "")
	c.Secrets.Token = getEnv("PROXY_SECRETS_TOKEN", "")
	c.Secrets.Namespace = getEnv("PROXY_SECRETS_NAMESPACE", "")
	c.Secrets.EnvPrefix = getEnv("PROXY_SECRETS_ENV_PREFIX", "PROXY_SECRET_")
	c.Secrets.CredentialEncryptionKey = getEnv("PROXY_CREDENTIAL_ENCRYPTION_KEY", "")

	c.Metrics.Enabled = getEnvBool("PROXY_METRICS_ENABLED", true)

	c.Logging.Level = getEnv("PROXY_LOGGING_LEVEL", "info")
	c.Logging.Format = getEnv("PROXY_LOGGING_FORMAT", "json")
	c.Logging.LogBodies = getEnvBool("PROXY_LOGGING_LOG_BODIES", false)

	c.Quota.ReservationTTL = getEnvDuration("PROXY_QUOTA_RESERVATION_TTL", 15*time.Minute)
	c.Quota.CodexDisabled = getEnvBool("PROXY_QUOTA_CODEX_DISABLED", false)

	c.Capture.Enabled = getEnvBool("PROXY_CAPTURE_ENABLED", true)
	c.Capture.Backend = getEnv("PROXY_CAPTURE_BACKEND", "postgres")
	c.Capture.RetentionDays = getEnvInt("PROXY_CAPTURE_RETENTION_DAYS", 30)
	c.Capture.QueueSize = getEnvInt("PROXY_CAPTURE_QUEUE_SIZE", 2000)
	c.Capture.Workers = getEnvInt("PROXY_CAPTURE_WORKERS", 4)
	// 0 = no truncation (full-fidelity capture). See CaptureConfig docs.
	c.Capture.MaxPayloadBytes = getEnvInt64("PROXY_CAPTURE_MAX_PAYLOAD_BYTES", 0)
	c.Capture.Strict = getEnvBool("PROXY_CAPTURE_STRICT", false)
	c.Capture.SessionLogRetentionDays = getEnvInt("PROXY_SESSION_LOG_RETENTION_DAYS", 30)

	c.SessionArchive.Enabled = getEnvBool("PROXY_SESSION_ARCHIVE_ENABLED", true)
	c.SessionArchive.ObjectsBackend = strings.ToLower(strings.TrimSpace(getEnv("PROXY_SESSION_OBJECTS_BACKEND", SessionObjectsPostgres)))
	c.SessionArchive.ObjectsBucket = strings.TrimSpace(getEnv("PROXY_SESSION_OBJECTS_BUCKET", ""))
	c.SessionArchive.ObjectsPrefix = strings.Trim(getEnv("PROXY_SESSION_OBJECTS_PREFIX", "piproxy"), "/")
	c.SessionArchive.RawBodies = strings.ToLower(strings.TrimSpace(getEnv("PROXY_SESSION_RAW_BODIES", "")))
	if c.SessionArchive.RawBodies == "" {
		c.SessionArchive.RawBodies = SessionRawBodiesPostgres
		if c.SessionArchive.ObjectsBackend == SessionObjectsGCS {
			c.SessionArchive.RawBodies = SessionRawBodiesObjects
		}
	}
	c.SessionArchive.MergeSegments = getEnvInt("PROXY_SESSION_ARCHIVE_MERGE_SEGMENTS", 32)
	c.SessionArchive.QueryURL = strings.TrimRight(strings.TrimSpace(getEnv("PROXY_SESSION_QUERY_URL", "")), "/")
	c.SessionArchive.QueryToken = getEnv("PROXY_SESSION_QUERY_TOKEN", "")
	c.SessionArchive.QueryTimeout = getEnvDuration("PROXY_SESSION_QUERY_TIMEOUT", 750*time.Millisecond)

	c.Usage.QueueSize = getEnvInt("PROXY_USAGE_QUEUE_SIZE", 4000)
	c.Usage.Workers = getEnvInt("PROXY_USAGE_WORKERS", 4)

	c.Hooks.EventsFile = getEnv("PROXY_HOOKS_EVENTS_FILE", "")
	c.Hooks.WebhookURL = getEnv("PROXY_HOOKS_WEBHOOK_URL", "")
	c.Hooks.WebhookSecret = getEnv("PROXY_HOOKS_WEBHOOK_SECRET", "")
	c.Hooks.QueueSize = getEnvInt("PROXY_HOOKS_QUEUE_SIZE", 1000)
	c.Hooks.Timeout = getEnvDuration("PROXY_HOOKS_TIMEOUT", 1500*time.Millisecond)
	c.Hooks.SubscriptionsEnabled = getEnvBool("PROXY_HOOKS_SUBSCRIPTIONS_ENABLED", false)
	c.Hooks.RefreshInterval = getEnvDuration("PROXY_HOOKS_REFRESH_INTERVAL", 30*time.Second)
	c.Hooks.BlockingBudget = getEnvDuration("PROXY_HOOKS_BLOCKING_BUDGET", 3*time.Second)
	c.Hooks.AllowPrivateEgress = getEnvBool("PROXY_HOOKS_ALLOW_PRIVATE_EGRESS", false)
	c.Hooks.DeliveryRetentionDays = getEnvInt("PROXY_HOOKS_DELIVERY_RETENTION_DAYS", 14)

	c.Providers.RefreshInterval = getEnvDuration("PROXY_PROVIDERS_REFRESH_INTERVAL", 30*time.Second)
	c.Providers.BalanceRefreshInterval = getEnvDuration("PROXY_PROVIDERS_BALANCE_REFRESH_INTERVAL", 5*time.Minute)
	c.Providers.BalanceRequestTimeout = getEnvDuration("PROXY_PROVIDERS_BALANCE_REQUEST_TIMEOUT", 10*time.Second)

	c.Upstream.Timeout = getEnvDuration("PROXY_UPSTREAM_TIMEOUT", 10*time.Minute)
	c.Upstream.MaxConnsPerHost = getEnvInt("PROXY_UPSTREAM_MAX_CONNS_PER_HOST", 100)
	c.Upstream.MaxIdleConns = getEnvInt("PROXY_UPSTREAM_MAX_IDLE_CONNS", 100)

	c.RateLimit.RequestsPerMinute = getEnvInt64("PROXY_RATELIMIT_REQUESTS_PER_MINUTE", 0)

	// Model guards: unset ships the deployment defaults; a set value must be
	// valid JSON or startup fails rather than silently guarding nothing.
	if raw := os.Getenv("PROXY_MODEL_GUARDS"); strings.TrimSpace(raw) != "" {
		guards, err := modelguard.Parse(raw)
		if err != nil {
			return fmt.Errorf("PROXY_MODEL_GUARDS: %w", err)
		}
		c.ModelGuards = guards
	} else {
		c.ModelGuards = modelguard.DefaultRules()
	}

	c.Duffel.Root = getEnv("PROXY_DUFFEL_ROOT", "")
	c.Duffel.URL = getEnv("PROXY_DUFFEL_URL", "")
	c.Duffel.Token = getEnv("PROXY_DUFFEL_TOKEN", "")
	c.Duffel.EnableSearch = getEnvBool("PROXY_DUFFEL_ENABLE_SEARCH", false)
	c.Duffel.Collection = getEnv("PROXY_DUFFEL_COLLECTION", "piproxy")

	c.Memory.ContextBudgetTokens = getEnvInt("PROXY_MEMORY_CONTEXT_BUDGET_TOKENS", 4000)
	c.Memory.ContextCacheTTL = getEnvDuration("PROXY_MEMORY_CONTEXT_CACHE_TTL", 2*time.Minute)
	c.Memory.SearchLimit = getEnvInt("PROXY_MEMORY_SEARCH_LIMIT", 5)
	c.Memory.SearchMinScore = getEnvFloat("PROXY_MEMORY_SEARCH_MIN_SCORE", 0)
	c.Memory.SearchIntent = getEnv("PROXY_MEMORY_SEARCH_INTENT", "")
	c.Memory.RecentLessons = getEnvInt("PROXY_MEMORY_RECENT_LESSONS", 10)
	c.Memory.MinBundleTokens = getEnvInt("PROXY_MEMORY_MIN_BUNDLE_TOKENS", 80)
	c.Memory.ObservationMaxBytes = getEnvInt("PROXY_MEMORY_OBSERVATION_MAX_BYTES", 4096)
	c.Memory.InjectionEnabled = getEnvBool("PROXY_MEMORY_INJECTION_ENABLED", false)
	c.Memory.InsightGenerationEnabled = getEnvBool("PROXY_INSIGHTS_ENABLED", getEnvBool("PROXY_MEMORY_DISTILLER_ENABLED", false))
	c.Memory.DistillerModel = getEnv("PROXY_MEMORY_DISTILLER_MODEL", "deepseek-v4-flash")
	c.Memory.DistillationQueueName = getEnv("PROXY_MEMORY_DISTILLATION_QUEUE_NAME", "memory_distillation")
	c.Memory.DistillationWorkers = getEnvInt("PROXY_MEMORY_DISTILLATION_WORKERS", 1)
	c.Memory.DistillationPoll = getEnvDuration("PROXY_MEMORY_DISTILLATION_POLL_INTERVAL", 30*time.Second)
	c.Memory.DistillationClaim = getEnvDuration("PROXY_MEMORY_DISTILLATION_CLAIM_TIMEOUT", 30*time.Minute)
	c.Memory.DistillationAttempts = getEnvInt("PROXY_MEMORY_DISTILLATION_MAX_ATTEMPTS", 3)
	c.Memory.DistillationNightlyUTC = getEnv("PROXY_MEMORY_DISTILLATION_NIGHTLY_AT_UTC", "23:00")
	c.Memory.DistillerIdle = getEnvDuration("PROXY_MEMORY_DISTILLER_IDLE", 15*time.Minute)
	c.Memory.CompactionEnabled = getEnvBool("PROXY_MEMORY_COMPACTION_ENABLED", true)
	c.Memory.CompactionInterval = getEnvDuration("PROXY_MEMORY_COMPACTION_INTERVAL", 6*time.Hour)
	c.Memory.CompactionThreshold = getEnvInt("PROXY_MEMORY_COMPACTION_THRESHOLD_TOKENS", 8000)
	c.Memory.CompactionMaxTokens = getEnvInt("PROXY_MEMORY_COMPACTION_MAX_TOKENS", 1200)
	c.Memory.CompactorModel = getEnv("PROXY_MEMORY_COMPACTOR_MODEL", "")
	c.Memory.CompactionQueue = getEnvBool("PROXY_MEMORY_COMPACTION_QUEUE", true)
	c.Memory.CompactionQueueName = getEnv("PROXY_MEMORY_COMPACTION_QUEUE_NAME", "memory_compaction")
	c.Memory.CompactionWorkers = getEnvInt("PROXY_MEMORY_COMPACTION_WORKERS", 1)
	c.Memory.CompactionPoll = getEnvDuration("PROXY_MEMORY_COMPACTION_POLL_INTERVAL", 30*time.Second)
	c.Memory.CompactionClaim = getEnvDuration("PROXY_MEMORY_COMPACTION_CLAIM_TIMEOUT", 30*time.Minute)
	c.Memory.CompactionPeakUTC = getEnv("PROXY_MEMORY_COMPACTION_PEAK_WINDOWS_UTC", "01:00-04:00,06:00-10:00")

	c.Web.Enabled = getEnvBool("PROXY_WEB_ENABLED", false)
	c.Web.APIKeyLogin = getEnvBool("PROXY_WEB_API_KEY_LOGIN", true)
	c.Web.BaseURL = getEnv("PROXY_WEB_BASE_URL", "")
	c.Web.OIDCIssuer = getEnv("PROXY_WEB_OIDC_ISSUER", "")
	c.Web.OIDCClientID = getEnv("PROXY_WEB_OIDC_CLIENT_ID", "")
	c.Web.OIDCClientSecret = getEnv("PROXY_WEB_OIDC_CLIENT_SECRET", "")
	c.Web.OIDCRedirectURL = getEnv("PROXY_WEB_OIDC_REDIRECT_URL", "")
	c.Web.OIDCProviderName = getEnv("PROXY_WEB_OIDC_PROVIDER_NAME", "your work account")
	c.Web.OIDCScopes = getEnv("PROXY_WEB_OIDC_SCOPES", "openid,profile,email")
	c.Web.SessionTTL = getEnvDuration("PROXY_WEB_SESSION_TTL", 12*time.Hour)
	c.Web.SessionIdleTTL = getEnvDuration("PROXY_WEB_SESSION_IDLE_TTL", 2*time.Hour)
	c.Web.CookieDomain = getEnv("PROXY_WEB_COOKIE_DOMAIN", "")
	c.Web.CookieInsecure = getEnvBool("PROXY_WEB_COOKIE_INSECURE", false)
	c.Web.MaxHistoryMessages = getEnvInt("PROXY_WEB_MAX_HISTORY_MESSAGES", 200)
	c.Web.AttachmentsBucket = getEnv("PROXY_WEB_ATTACHMENTS_BUCKET", "")
	c.Web.AttachmentsPrefix = getEnv("PROXY_WEB_ATTACHMENTS_PREFIX", "attachments")
	c.Web.AttachmentMaxBytes = getEnvInt64("PROXY_WEB_ATTACHMENT_MAX_BYTES", 25*1024*1024)
	c.Web.AttachmentRetention = getEnvInt("PROXY_WEB_ATTACHMENT_RETENTION_DAYS", 90)
	c.Web.AttachmentURLTTL = getEnvDuration("PROXY_WEB_ATTACHMENT_URL_TTL", 10*time.Minute)
	c.Web.AttachmentPendingTTL = getEnvDuration("PROXY_WEB_ATTACHMENT_PENDING_TTL", time.Hour)
	c.Web.AttachmentUserBytes = getEnvInt64("PROXY_WEB_ATTACHMENT_USER_BYTES", 1<<30)
	c.Web.AttachmentTenantBytes = getEnvInt64("PROXY_WEB_ATTACHMENT_TENANT_BYTES", 10<<30)
	c.Web.AttachmentPendingPerUser = getEnvInt64("PROXY_WEB_ATTACHMENT_PENDING_PER_USER", 16)
	c.Web.WriteRequestsPerMinute = getEnvInt64("PROXY_WEB_WRITE_REQUESTS_PER_MINUTE", 60)

	return nil
}

// ValidateProductionSafety rejects configuration combinations that are unsafe
// for an internet-exposed public proxy. Local and private-network Postgres DSNs
// may still use sslmode=disable for dev/Tailscale-style deployments.
func (c *Config) ValidateProductionSafety() error {
	if c == nil || !c.publicProxyExposed() {
		return nil
	}
	host, sslmode := postgresHostAndSSLMode(c.Postgres.DSN)
	if !postgresSSLModeUnsafe(sslmode) || trustedPostgresHost(host) {
		return nil
	}
	return fmt.Errorf("unsafe postgres sslmode %q for internet-exposed public proxy and remote host %q; use sslmode=require, verify-ca, or verify-full", sslmodeForMessage(sslmode), host)
}

func (c *Config) publicProxyExposed() bool {
	if publicBaseURLInternetExposed(c.Server.PublicBaseURL) {
		return true
	}
	return c.RateLimit.RequestsPerMinute > 0 && publicAddrPotentiallyExternal(c.Server.PublicAddr)
}

func publicBaseURLInternetExposed(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if host == "" {
		host = hostWithoutPort(raw)
	}
	return !trustedPostgresHost(host)
}

func publicAddrPotentiallyExternal(addr string) bool {
	host := hostWithoutPort(addr)
	if host == "" {
		return true
	}
	return !trustedPostgresHost(host)
}

func postgresHostAndSSLMode(dsn string) (host, sslmode string) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", ""
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", ""
		}
		return u.Hostname(), strings.ToLower(u.Query().Get("sslmode"))
	}
	fields := parseKeywordDSN(dsn)
	host = fields["host"]
	sslmode = strings.ToLower(fields["sslmode"])
	return hostWithoutPort(host), sslmode
}

func parseKeywordDSN(dsn string) map[string]string {
	fields := make(map[string]string)
	for _, part := range strings.Fields(dsn) {
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		fields[strings.ToLower(key)] = strings.Trim(value, `'"`)
	}
	return fields
}

func postgresSSLModeUnsafe(sslmode string) bool {
	switch strings.ToLower(strings.TrimSpace(sslmode)) {
	case "require", "verify-ca", "verify-full":
		return false
	default:
		return true
	}
}

func sslmodeForMessage(sslmode string) string {
	if strings.TrimSpace(sslmode) == "" {
		return "<empty>"
	}
	return sslmode
}

func trustedPostgresHost(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" || strings.HasPrefix(host, "/") || host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return true
	}
	_, tailscale, _ := net.ParseCIDR("100.64.0.0/10")
	return tailscale.Contains(ip)
}

func hostWithoutPort(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "/") {
		return value
	}
	host, _, err := net.SplitHostPort(value)
	if err == nil {
		return host
	}
	if strings.Count(value, ":") == 1 {
		if host, _, ok := strings.Cut(value, ":"); ok {
			return host
		}
	}
	return value
}

// helpers

func getEnv(key, defaultVal string) string {
	if val, ok := os.LookupEnv(key); ok {
		return val
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) int {
	val, ok := os.LookupEnv(key)
	if !ok {
		return defaultVal
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return defaultVal
	}
	return n
}

func getEnvInt64(key string, defaultVal int64) int64 {
	val, ok := os.LookupEnv(key)
	if !ok {
		return defaultVal
	}
	n, err := strconv.ParseInt(val, 10, 64)
	if err != nil {
		return defaultVal
	}
	return n
}

func getEnvBool(key string, defaultVal bool) bool {
	val, ok := os.LookupEnv(key)
	if !ok {
		return defaultVal
	}
	switch strings.ToLower(val) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		return defaultVal
	}
}

func getEnvFloat(key string, defaultVal float64) float64 {
	val, ok := os.LookupEnv(key)
	if !ok {
		return defaultVal
	}
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return defaultVal
	}
	return f
}

func getEnvDuration(key string, defaultVal time.Duration) time.Duration {
	val, ok := os.LookupEnv(key)
	if !ok {
		return defaultVal
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return defaultVal
	}
	return d
}
