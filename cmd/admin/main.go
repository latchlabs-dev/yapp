// Command admin is the admin API server for the Pi proxy.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/earlgrey/piproxy/internal/audit"
	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/codexauth"
	"github.com/earlgrey/piproxy/internal/config"
	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/httpapi/admin"
	"github.com/earlgrey/piproxy/internal/insights"
	"github.com/earlgrey/piproxy/internal/logging"
	"github.com/earlgrey/piproxy/internal/memory"
	"github.com/earlgrey/piproxy/internal/providerbalance"
	"github.com/earlgrey/piproxy/internal/secrets"
	"github.com/earlgrey/piproxy/internal/sessionstorage"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	redisstore "github.com/earlgrey/piproxy/internal/store/redis"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	logCfg := logging.Config{
		Level:  cfg.Logging.Level,
		Format: cfg.Logging.Format,
	}
	logger := logging.NewLogger(logCfg)

	logger.Info().
		Str("version", config.Version).
		Str("commit", config.Commit).
		Str("addr", cfg.Server.AdminAddr).
		Msg("starting admin server")

	// Global context for shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- Initialize Postgres (required for admin). ---
	var pgPool *pgstore.Pool
	if cfg.Postgres.DSN != "" {
		pgCtx, pgCancel := context.WithTimeout(ctx, 10*time.Second)
		pool, err := pgstore.New(pgCtx, pgstore.Config{
			DSN:          cfg.Postgres.DSN,
			MaxOpenConns: cfg.Postgres.MaxOpenConns,
			MaxIdleConns: cfg.Postgres.MaxIdleConns,
		})
		pgCancel()
		if err != nil {
			logger.Fatal().Err(err).Msg("postgres is required for admin server")
		}
		pgPool = pool
		defer pgPool.Close()
		logger.Info().Msg("connected to postgres")
	} else {
		logger.Fatal().Msg("postgres DSN is required for admin server")
	}

	// Session archive: admin reads what the proxy writes, so both processes
	// must be configured with the same PROXY_SESSION_* settings.
	sessionServices, err := sessionstorage.Open(ctx, cfg.SessionArchive, pgPool, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("failed to open session archive storage")
	}
	defer func() { _ = sessionServices.Close() }()
	logger.Info().Str("objects", sessionServices.Objects.Location()).Msg("session archive storage initialized")

	// --- Initialize Audit Logger ---
	auditLogger := audit.NewLogger(100)
	go func() {
		for event := range auditLogger.Events() {
			logger.Info().
				Str("action", event.Action).
				Str("actor", event.ActorID).
				Str("target_type", event.TargetType).
				Str("target_id", event.TargetID).
				Str("result", event.Result).
				Msg("admin audit event")
		}
	}()
	logger.Info().Msg("admin audit logger initialized")

	// --- Initialize Redis (optional, used for revocation cache and live
	// quota-counter reads). ---
	var revocationCache *redisstore.RevocationCache
	var redisClient *redisstore.Client
	if cfg.Redis.Address != "" {
		rc, err := redisstore.New(ctx, redisstore.Config{
			Address:  cfg.Redis.Address,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
			TLS:      cfg.Redis.TLS,
		})
		if err != nil {
			logger.Warn().Err(err).Msg("redis not available, revocation cache disabled")
		} else {
			redisClient = rc
			revocationCache = redisstore.NewRevocationCache(rc)
			logger.Info().Str("revocation", revocationCache.String()).Msg("revocation cache initialized")
		}
	} else {
		logger.Info().Msg("no redis address configured, revocation cache disabled")
	}

	// Initialize the external secret store once. It supplies the key-hash
	// pepper and acts as a fallback for non-DB provider credentials.
	secretStore, err := secrets.New(
		cfg.Secrets.Provider,
		cfg.Secrets.Address,
		cfg.Secrets.Token,
		cfg.Secrets.Namespace,
		cfg.Secrets.EnvPrefix,
	)
	if err != nil {
		logger.Warn().Err(err).Msg("secret store not available")
	}

	// --- Configure key-hash pepper ---
	// MUST match the proxy: the admin hashes new API keys with this pepper, and
	// the proxy verifies incoming keys with it. If they disagree, every key the
	// admin issues is rejected by the proxy with "invalid API key".
	{
		pepper := ""
		if cfg.Auth.HashPepperRef != "" {
			if secretStore != nil {
				if data, err := secretStore.Get(ctx, cfg.Auth.HashPepperRef); err == nil {
					pepper = string(data)
				}
			}
			if pepper == "" {
				pepper = os.Getenv("PROXY_KEY_HASH_PEPPER")
			}
		}
		if pepper == "" {
			logger.Warn().Msg("no key-hash pepper configured; keys will be hashed with SHA-256 (must match proxy)")
		} else {
			logger.Info().Msg("key-hash pepper configured")
		}
		auth.SetPepper(pepper)
	}

	// --- Build Dependencies ---
	adminKey := os.Getenv("PROXY_ADMIN_KEY")
	if adminKey == "" {
		logger.Warn().Msg("PROXY_ADMIN_KEY not set; admin API will reject all requests")
	}

	var credentialEncryptor *secrets.Encryptor
	if cfg.Secrets.CredentialEncryptionKey != "" {
		credentialEncryptor, err = secrets.NewEncryptorFromKeyMaterial(cfg.Secrets.CredentialEncryptionKey)
		if err != nil {
			logger.Fatal().Err(err).Msg("invalid credential encryption key")
		}
		logger.Info().Msg("credential DB encryption initialized")
	} else {
		logger.Warn().Msg("PROXY_CREDENTIAL_ENCRYPTION_KEY not set; credential upload is disabled")
	}

	// --- Periodic upstream provider balance refresh ---
	// The worker reads encrypted credentials through the same OAuth-aware path
	// as the data plane, so a Codex refresh-token rotation remains durable.
	var balanceWorker *providerbalance.Worker
	providerBalanceStore := pgstore.NewProviderBalanceStore(pgPool)
	if credentialEncryptor != nil {
		providerCredentialStore := pgstore.NewProviderCredentialStore(pgPool)
		var balanceSecretStore secrets.SecretStore = secrets.NewEncryptedDBStore(providerCredentialStore, credentialEncryptor)
		if secretStore != nil {
			balanceSecretStore = secrets.NewFallbackStore(balanceSecretStore, secretStore)
		}
		credentialStore := secrets.NewCredentialStore(balanceSecretStore)
		credentialStore.SetLogger(logger)
		credentialStore.SetOAuthRefresher(codexauth.NewRefresher())
		credentialStore.SetPersister(secrets.NewDBCredentialPersister(providerCredentialStore, credentialEncryptor, logger))
		resolver := providerbalance.CredentialResolverFunc(func(resolveCtx context.Context, ref string) (*providerbalance.Credential, error) {
			credential, err := credentialStore.GetCredential(resolveCtx, ref)
			if err != nil {
				return nil, err
			}
			return &providerbalance.Credential{Secret: credential.SecretValue(), AccountID: credential.AccountID}, nil
		})
		balanceWorker = providerbalance.NewWorker(
			providerBalanceStore,
			resolver,
			&http.Client{Timeout: cfg.Providers.BalanceRequestTimeout},
			cfg.Providers.BalanceRefreshInterval,
			logger,
		)
		balanceWorker.Start(ctx)
		logger.Info().Dur("interval", cfg.Providers.BalanceRefreshInterval).Msg("provider balance refresh started")
	} else {
		logger.Warn().Msg("provider balance refresh disabled; credential encryption is unavailable")
	}

	// --- Daily fixed-subscription rate cards ---
	// Today's card values provider-wide utilization movement over the trailing
	// 24 hours and allocates it across proxied tokens from the same interval.
	// This deliberately includes subscription use that bypassed PiProxy.
	subscriptionRateStore := pgstore.NewSubscriptionRateStore(pgPool)
	subscriptionRateCalculator := subscriptioncost.NewCalculator(subscriptionRateStore)
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if err := subscriptionRateStore.ReconcileObservedSubscriptions(ctx, time.Now()); err != nil {
				logger.Error().Err(err).Msg("observed subscription allocation failed")
			}
			if redisClient != nil {
				if err := subscriptionRateStore.FlushSubscriptionComponents(ctx, redisClient, time.Now()); err != nil {
					logger.Error().Err(err).Msg("subscription quota reconciliation failed")
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	calculateSubscriptionRates := func() {
		if err := subscriptionRateCalculator.CalculateAll(ctx, time.Now()); err != nil {
			logger.Error().Err(err).Msg("subscription rate-card calculation failed")
		}
	}
	calculateSubscriptionRates()
	go func() {
		for {
			now := time.Now().UTC()
			next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 1, 0, 0, time.UTC)
			timer := time.NewTimer(time.Until(next))
			select {
			case <-timer.C:
				calculateSubscriptionRates()
			case <-ctx.Done():
				timer.Stop()
				return
			}
		}
	}()

	// Duffel library workspace for the project-memory knowledge browser (optional).
	var duffelClient *duffel.Client
	if cfg.Duffel.Root != "" {
		duffelClient = duffel.New(duffel.Config{
			Root:         cfg.Duffel.Root,
			EnableSearch: cfg.Duffel.EnableSearch,
			Collection:   cfg.Duffel.Collection,
		})
		logger.Info().Str("duffel_root", cfg.Duffel.Root).Msg("memory knowledge browser enabled")
	} else if cfg.Duffel.URL != "" {
		logger.Warn().Str("duffel_url", cfg.Duffel.URL).Msg("PROXY_DUFFEL_URL is deprecated and ignored; set PROXY_DUFFEL_ROOT to enable project memory")
	}

	// Bundle builder powers the read-only "what the agent actually sees" preview.
	// It assembles bundles exactly as the proxy does, over the same workspace, so
	// operators can spot silent degradation (empty bundle when duffel is down).
	var memoryBuilder *memory.Builder
	if duffelClient != nil {
		memoryBuilder = memory.NewBuilder(duffelClient, memory.BundleConfig{}, nil, logger)
	}

	// Hook subscriptions are admin-managed here and hot-reloaded by the proxy.
	// A typed-nil encryptor would satisfy the store's interface and then be
	// called, so the nil case passes a literal nil.
	var hookSubscriptionStore *pgstore.HookSubscriptionStore
	if credentialEncryptor != nil {
		hookSubscriptionStore = pgstore.NewHookSubscriptionStore(pgPool, credentialEncryptor)
	} else {
		hookSubscriptionStore = pgstore.NewHookSubscriptionStore(pgPool, nil)
	}

	userStore := pgstore.NewUserStore(pgPool)
	insightClients := insights.NewClientPool()
	defer insightClients.Close()
	deps := &admin.Dependencies{
		Logger:                   logger,
		TenantStore:              pgstore.NewTenantStore(pgPool),
		UserStore:                userStore,
		KeyStore:                 pgstore.NewKeyStore(pgPool),
		QuotaPolicyStore:         pgstore.NewQuotaPolicyStore(pgPool),
		ProviderStore:            pgstore.NewProviderStore(pgPool),
		ProviderBalanceStore:     providerBalanceStore,
		SubscriptionRateStore:    subscriptionRateStore,
		ProviderCredentialStore:  pgstore.NewProviderCredentialStore(pgPool),
		ModelAliasStore:          pgstore.NewModelAliasStore(pgPool),
		TenantModelSettingsStore: pgstore.NewTenantModelSettingsStore(pgPool),
		UsageStore:               pgstore.NewUsageStore(pgPool),
		ConversationPayloadStore: sessionServices.Payloads,
		CaptureGapStore:          pgstore.NewCaptureGapStore(pgPool),
		SessionArchive:           sessionServices,
		SessionSubmissionStore:   pgstore.NewSessionSubmissionStore(pgPool),
		ExtensionErrorStore:      pgstore.NewExtensionErrorStore(pgPool),
		InsightStore:             pgstore.NewInsightStore(pgPool),
		InsightClients:           insightClients,
		MemoryProjectStore:       pgstore.NewMemoryProjectStore(pgPool),
		ProjectUserStore:         userStore,
		AuditLogger:              auditLogger,
		AdminKey:                 adminKey,
		RevocationCache:          revocationCache,
		RedisClient:              redisClient,
		CredentialEncryptor:      credentialEncryptor,
		MemoryRuleStore:          pgstore.NewMemoryRuleStore(pgPool),
		HookSubscriptionStore:    hookSubscriptionStore,
		WebIdentityStore:         pgstore.NewWebIdentityStore(pgPool),
		WebSessionStore:          pgstore.NewWebSessionStore(pgPool),
		AllowPrivateHookEgress:   cfg.Hooks.AllowPrivateEgress,
		DuffelClient:             duffelClient,
		MemoryBuilder:            memoryBuilder,
		ProviderBalanceRefresher: balanceWorker,
		PublicBaseURL:            cfg.Server.PublicBaseURL,
	}

	// --- Create Admin Router ---
	router := admin.NewRouter(deps)

	// --- Create HTTP Server ---
	server := &http.Server{
		Addr:         cfg.Server.AdminAddr,
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// Start server.
	go func() {
		logger.Info().Str("addr", cfg.Server.AdminAddr).Msg("admin server listening")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal().Err(err).Msg("admin server error")
		}
	}()

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh

	logger.Info().Str("signal", sig.String()).Msg("shutting down admin server")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error().Err(err).Msg("admin server shutdown error")
	}

	logger.Info().Msg("admin server stopped")
	fmt.Println("admin server stopped gracefully")
}
