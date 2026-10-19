package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/config"
	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/httpapi/anthropic"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/httpapi/quotausage"
	"github.com/earlgrey/piproxy/internal/httpapi/webui"
	"github.com/earlgrey/piproxy/internal/memory"
	"github.com/earlgrey/piproxy/internal/modelaccess"
	"github.com/earlgrey/piproxy/internal/modelguard"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/ratelimit"
	"github.com/earlgrey/piproxy/internal/secrets"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	redisstore "github.com/earlgrey/piproxy/internal/store/redis"
	"github.com/earlgrey/piproxy/internal/usage"
	"github.com/earlgrey/piproxy/internal/webauth"
	"github.com/earlgrey/piproxy/internal/weborigin"
)

// webConsoleInputs are the already-constructed services the console shares with
// the rest of the data plane.
type webConsoleInputs struct {
	Pool             *pgstore.Pool
	RevocationCache  *redisstore.RevocationCache
	Registry         *providers.Registry
	ModelAccess      modelaccess.Store
	ModelGuards      modelguard.Rules
	QuotaManager     *quota.Manager
	QuotaCounter     quotausage.CounterReader
	CacheTrendReader usage.CacheTrendReader
	MemoryBuilder    *memory.Builder
	Duffel           *duffel.Client
	Projects         *pgstore.MemoryProjectStore
	Hooks            *hooks.Manager
}

// webConsole is the assembled console: a router factory to hand the public
// router, plus the resources it owns and must release on shutdown.
type webConsole struct {
	Factory func(*openai.ChatHandler, *anthropic.MessagesHandler) http.Handler
	deps    *webui.Dependencies
	blobs   blob.Store
}

// Close releases the console's own resources.
func (c *webConsole) Close() {
	if c != nil && c.blobs != nil {
		_ = c.blobs.Close()
	}
}

// setupWebConsole builds the private portal, or returns nil when disabled.
// Invalid origin/encryption settings or configured-but-broken OIDC fail startup;
// existing-key login does not depend on OIDC discovery.
func setupWebConsole(ctx context.Context, cfg *config.Config, in webConsoleInputs, logger zerolog.Logger) (*webConsole, error) {
	if !cfg.Web.Enabled {
		return nil, nil
	}
	if !cfg.Web.Ready() {
		return nil, fmt.Errorf("web portal requires a valid HTTPS PROXY_WEB_BASE_URL, a login method, host-only key cookies, and complete optional OIDC configuration")
	}
	if in.Pool == nil {
		return nil, fmt.Errorf("web console requires Postgres")
	}

	origin, _ := weborigin.Canonical(cfg.Web.BaseURL) // Ready validated it above.
	cookie := webauth.CookieOptions{
		Domain:   cfg.Web.CookieDomain,
		Insecure: cfg.Web.CookieInsecure,
	}

	// OIDC is optional: existing-key login does not require an identity
	// provider or an email-domain mapping. Configured OIDC still fails closed.
	var provider *webauth.Provider
	if cfg.Web.OIDCReady() {
		discoveryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		var err error
		provider, err = webauth.NewProvider(discoveryCtx, webauth.Config{
			Issuer: cfg.Web.OIDCIssuer, ClientID: cfg.Web.OIDCClientID,
			ClientSecret: cfg.Web.OIDCClientSecret, RedirectURL: cfg.Web.OIDCRedirectURL,
			Scopes: splitAndTrim(cfg.Web.OIDCScopes), Cookie: cookie,
		})
		if err != nil {
			return nil, fmt.Errorf("web console oidc: %w", err)
		}
	}

	identities := pgstore.NewWebIdentityStore(in.Pool)
	sessions := webauth.NewSessionManager(
		identities,
		pgstore.NewTenantStore(in.Pool),
		pgstore.NewUserStore(in.Pool),
		pgstore.NewWebSessionStore(in.Pool),
		cfg.Web.SessionTTL,
		cfg.Web.SessionIdleTTL,
	)

	var keySessions *webauth.KeySessions
	if cfg.Web.APIKeyLogin {
		cipher, err := secrets.NewEncryptorFromKeyMaterial(cfg.Secrets.CredentialEncryptionKey)
		if err != nil {
			return nil, fmt.Errorf("web key cookie encryption: %w", err)
		}
		keySessions = webauth.NewKeySessions(pgstore.NewKeyStore(in.Pool),
			pgstore.NewTenantStore(in.Pool), pgstore.NewUserStore(in.Pool),
			in.RevocationCache, cipher, cfg.Web.SessionTTL)
		sessions.EnableKeyLogin(keySessions)
	}

	var blobs blob.Store
	if cfg.Web.AttachmentsReady() {
		gcs, err := blob.NewGCS(ctx, cfg.Web.AttachmentsBucket, cfg.Web.AttachmentsPrefix)
		if err != nil {
			return nil, fmt.Errorf("web console attachments: %w", err)
		}
		blobs = gcs
		logger.Info().Str("bucket", cfg.Web.AttachmentsBucket).Msg("web console attachments initialized")
	} else {
		logger.Warn().Msg("web console attachments disabled: PROXY_WEB_ATTACHMENTS_BUCKET is unset")
	}

	deps := &webui.Dependencies{
		Config: webui.Config{
			Enabled:                  true,
			BaseURL:                  origin,
			ProviderName:             cfg.Web.OIDCProviderName,
			Cookie:                   cookie,
			SessionTTL:               cfg.Web.SessionTTL,
			MaxHistoryMessages:       cfg.Web.MaxHistoryMessages,
			AttachmentsBucket:        cfg.Web.AttachmentsBucket,
			AttachmentsPrefix:        cfg.Web.AttachmentsPrefix,
			AttachmentMaxBytes:       cfg.Web.AttachmentMaxBytes,
			AttachmentRetention:      cfg.Web.AttachmentRetention,
			AttachmentURLTTL:         cfg.Web.AttachmentURLTTL,
			AttachmentUserBytes:      cfg.Web.AttachmentUserBytes,
			AttachmentTenantBytes:    cfg.Web.AttachmentTenantBytes,
			AttachmentPendingPerUser: cfg.Web.AttachmentPendingPerUser,
			// The write guard is per-process and in-memory: it exists to bound
			// database growth, not to meter billing, so it degrades open if the
			// store is ever absent and does not need Redis.
			WriteRequestsPerMinute: cfg.Web.WriteRequestsPerMinute,
			MemoryBudgetTokens:     cfg.Memory.ContextBudgetTokens,
		},
		Logger:           logger,
		OIDC:             provider,
		Sessions:         sessions,
		KeySessions:      keySessions,
		Tenants:          pgstore.NewTenantStore(in.Pool),
		Users:            pgstore.NewUserStore(in.Pool),
		Identities:       identities,
		Conversations:    pgstore.NewWebConversationStore(in.Pool),
		Attachments:      pgstore.NewWebAttachmentStore(in.Pool),
		Projects:         in.Projects,
		Registry:         in.Registry,
		ModelAccess:      in.ModelAccess,
		ModelGuards:      in.ModelGuards,
		QuotaManager:     in.QuotaManager,
		QuotaCounter:     in.QuotaCounter,
		CacheTrendReader: in.CacheTrendReader,
		MemoryBuilder:    in.MemoryBuilder,
		Duffel:           in.Duffel,
		Blobs:            blobs,
		Hooks:            in.Hooks,
		WriteLimiter:     ratelimit.NewMemoryStore(),
	}

	console := &webConsole{deps: deps, blobs: blobs}
	console.Factory = func(chat *openai.ChatHandler, msg *anthropic.MessagesHandler) http.Handler {
		deps.ChatHandler = chat
		deps.MsgHandler = msg
		return webui.NewRouter(deps)
	}
	logger.Info().Bool("api_key_login", keySessions != nil).Bool("oidc_login", provider != nil).Msg("private web portal initialized")
	return console, nil
}

// startWebConsoleCleanup sweeps expired sessions and attachments every five minutes.
// Attachment objects are deleted before their rows, so a failure leaves an
// orphaned row the next sweep retries rather than an orphaned object nothing
// will ever look at again.
func startWebConsoleCleanup(ctx context.Context, wg *sync.WaitGroup, console *webConsole, pool *pgstore.Pool, pendingTTL time.Duration, logger zerolog.Logger) {
	if console == nil || pool == nil {
		return
	}
	sessions := pgstore.NewWebSessionStore(pool)
	attachments := pgstore.NewWebAttachmentStore(pool)
	if pendingTTL <= 0 {
		pendingTTL = time.Hour
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			sweepWebConsole(ctx, sessions, attachments, console.blobs, pendingTTL, logger)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func sweepWebConsole(ctx context.Context, sessions *pgstore.WebSessionStore, attachments *pgstore.WebAttachmentStore, blobs blob.Store, pendingTTL time.Duration, logger zerolog.Logger) {
	sweepCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	if removed, err := sessions.DeleteExpired(sweepCtx); err != nil {
		logger.Warn().Err(err).Msg("web session cleanup failed")
	} else if removed > 0 {
		logger.Info().Int64("sessions", removed).Msg("web sessions swept")
	}

	if blobs == nil {
		return
	}
	for sweepCtx.Err() == nil {
		stale, err := attachments.ListSweepable(sweepCtx, pendingTTL, 500)
		if err != nil {
			logger.Warn().Err(err).Msg("web attachment cleanup listing failed")
			return
		}
		removable := make([]string, 0, len(stale))
		for _, a := range stale {
			if err := blobs.Delete(sweepCtx, a.ObjectKey); err != nil {
				logger.Warn().Err(err).Str("attachment_id", a.ID).Msg("web attachment object delete failed")
				continue
			}
			removable = append(removable, a.ID)
		}
		if err := attachments.Delete(sweepCtx, removable); err != nil {
			logger.Warn().Err(err).Msg("web attachment row delete failed")
			return
		}
		if len(removable) > 0 {
			logger.Info().Int("attachments", len(removable)).Msg("web attachments swept")
		}
		if len(stale) < 500 || len(removable) == 0 {
			return
		}
	}
}

// splitAndTrim parses a comma-separated config value into a clean slice.
func splitAndTrim(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// webConsoleFactory adapts a possibly-nil console into the router's optional
// factory field, so the router stays free of nil-console special cases.
func webConsoleFactory(console *webConsole) func(*openai.ChatHandler, *anthropic.MessagesHandler) http.Handler {
	if console == nil {
		return nil
	}
	return console.Factory
}

// marshalHookDecision renders a hook's decision for the delivery log. A
// decision that cannot be encoded is logged as absent rather than failing the
// record: the log is diagnostics, and losing one field beats losing the row.
func marshalHookDecision(decision *hooks.Decision) json.RawMessage {
	if decision == nil {
		return nil
	}
	encoded, err := json.Marshal(decision)
	if err != nil {
		return nil
	}
	return encoded
}

// startHookDeliveryCleanup trims the hook delivery log daily.
func startHookDeliveryCleanup(ctx context.Context, wg *sync.WaitGroup, store *pgstore.HookSubscriptionStore, retentionDays int, logger zerolog.Logger) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			sweepCtx, cancel := context.WithTimeout(ctx, time.Minute)
			if removed, err := store.DeleteExpiredDeliveries(sweepCtx, retentionDays); err != nil {
				logger.Warn().Err(err).Msg("hook delivery cleanup failed")
			} else if removed > 0 {
				logger.Info().Int64("deliveries", removed).Msg("hook deliveries swept")
			}
			cancel()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
