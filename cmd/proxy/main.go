// Command proxy is the public data-plane server for the Pi proxy.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/audit"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/codexauth"
	"github.com/earlgrey/piproxy/internal/config"
	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/httpapi"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/memorycontext"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/insights"
	"github.com/earlgrey/piproxy/internal/llmclient"
	"github.com/earlgrey/piproxy/internal/logging"
	"github.com/earlgrey/piproxy/internal/memory"
	"github.com/earlgrey/piproxy/internal/metrics"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/ratelimit"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/secrets"
	"github.com/earlgrey/piproxy/internal/sessionstorage"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	redisstore "github.com/earlgrey/piproxy/internal/store/redis"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
	"github.com/earlgrey/piproxy/internal/upstream"
	"github.com/earlgrey/piproxy/internal/usage"
)

func main() {
	// Load configuration.
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}
	if err := cfg.ValidateProductionSafety(); err != nil {
		fmt.Fprintf(os.Stderr, "unsafe proxy config: %v\n", err)
		os.Exit(1)
	}

	// Initialize logger.
	logCfg := logging.Config{
		Level:     cfg.Logging.Level,
		Format:    cfg.Logging.Format,
		LogBodies: cfg.Logging.LogBodies,
	}
	logger := logging.NewLogger(logCfg)
	logger.Info().
		Str("version", config.Version).
		Str("commit", config.Commit).
		Str("build_date", config.BuildDate).
		Bool("codex_quota_disabled", cfg.Quota.CodexDisabled).
		Msg("starting proxy server")

	// Global context for shutdown.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Two WaitGroups govern ordered shutdown (see the shutdown block at the end
	// of main):
	//   - drainWg tracks the data-loss-sensitive async consumers
	//     (usage/audit/capture/hooks). They are stopped by CLOSING their intake
	//     channel, drain remaining buffered events, and are Waited BEFORE the
	//     root context is cancelled.
	//   - sweeperWg tracks the long-running sweepers (distiller, compactor,
	//     quota-policy refresh, capture-retention cleanup). They watch
	//     ctx.Done() and are Waited AFTER cancel().
	// The two cannot share one WaitGroup: sweepers only stop on cancel(), so
	// waiting on them before cancel() would deadlock the drain group.
	var drainWg sync.WaitGroup
	var sweeperWg sync.WaitGroup

	// drainCtx holds the bounded context used by the drain-group consumers for
	// their store writes. It is nil during normal operation (consumers fall
	// back to the root ctx) and is replaced with a fresh, NOT-root-derived
	// context at shutdown so the final flush is not cancelled by cancel().
	var drainCtx atomic.Pointer[context.Context]
	consumerCtx := func() context.Context {
		if c := drainCtx.Load(); c != nil {
			return *c
		}
		return ctx
	}

	// --- Initialize Postgres ---
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
			logger.Fatal().Err(err).Msg("postgres unavailable; refusing to start public proxy without auth")
		} else {
			pgPool = pool
			defer pgPool.Close()
			logger.Info().Msg("connected to postgres")
		}
	} else {
		logger.Fatal().Msg("postgres DSN is required; refusing to start public proxy without auth")
	}

	// --- Initialize Redis ---
	// Fast-fail: Redis pool uses 1s dial timeout; empty address skips entirely.
	redisClient, err := redisstore.New(ctx, redisstore.Config{
		Address:  cfg.Redis.Address,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
		TLS:      cfg.Redis.TLS,
	})
	if err != nil {
		logger.Fatal().Err(err).Msg("redis unavailable; refusing to start public proxy without distributed quotas")
	}
	if !redisClient.IsConnected() {
		logger.Fatal().Msg("redis unavailable; refusing to start public proxy without distributed quotas")
	}
	defer redisClient.Close()
	logger.Info().Msg("connected to redis")

	// --- Initialize Secret Store ---
	secretStore, err := secrets.New(
		cfg.Secrets.Provider,
		cfg.Secrets.Address,
		cfg.Secrets.Token,
		cfg.Secrets.Namespace,
		cfg.Secrets.EnvPrefix,
	)
	if err != nil {
		logger.Warn().Err(err).Msg("secret store not available, running without secrets")
	} else {
		logger.Info().Str("provider", cfg.Secrets.Provider).Msg("secret store initialized")
	}
	var credentialStore *secrets.CredentialStore
	var credentialSecretStore secrets.SecretStore = secretStore
	if pgPool != nil {
		if cfg.Secrets.CredentialEncryptionKey != "" {
			credentialEncryptor, err := secrets.NewEncryptorFromKeyMaterial(cfg.Secrets.CredentialEncryptionKey)
			if err != nil {
				logger.Fatal().Err(err).Msg("invalid credential encryption key")
			}
			providerCredentialStore := pgstore.NewProviderCredentialStore(pgPool)
			dbCredentialStore := secrets.NewEncryptedDBStore(providerCredentialStore, credentialEncryptor)
			if credentialSecretStore != nil {
				credentialSecretStore = secrets.NewFallbackStore(dbCredentialStore, credentialSecretStore)
			} else {
				credentialSecretStore = dbCredentialStore
			}
			logger.Info().Msg("encrypted DB credential store initialized")
		} else {
			logger.Warn().Msg("PROXY_CREDENTIAL_ENCRYPTION_KEY not set; encrypted DB provider credentials disabled")
		}
	}
	if credentialSecretStore != nil {
		credentialStore = secrets.NewCredentialStore(credentialSecretStore)
		credentialStore.SetLogger(logger)
		// Codex (ChatGPT) credentials use rotating OAuth2 refresh tokens. Attach
		// a refresher + DB persister so the proxy renews expired access tokens
		// and saves the rotated refresh token back to provider_credentials.
		credentialStore.SetOAuthRefresher(codexauth.NewRefresher())
		if pgPool != nil && cfg.Secrets.CredentialEncryptionKey != "" {
			enc, _ := secrets.NewEncryptorFromKeyMaterial(cfg.Secrets.CredentialEncryptionKey)
			credentialStore.SetPersister(secrets.NewDBCredentialPersister(
				pgstore.NewProviderCredentialStore(pgPool), enc, logger,
			))
		}
	}

	// --- Initialize Provider Registry ---
	providerRegistry := providers.NewRegistry()
	for _, p := range providers.DefaultProviders() {
		if err := providerRegistry.RegisterProvider(p); err != nil {
			logger.Fatal().Err(err).Msg("failed to register default provider")
		}
	}
	logger.Info().Int("count", len(providerRegistry.ListProviders())).Msg("providers registered")

	// Start periodic provider config refresh from Postgres.
	providerLoader := providers.NewConfigLoader(providerRegistry)
	var providerRefresher *providers.Refresher
	if pgPool != nil {
		providerStore := pgstore.NewProviderStore(pgPool)
		providerLoader.SetStore(providerStore)
		providerRefresher = providers.NewRefresher(providerLoader, cfg.Providers.RefreshInterval, logger)
		providerRefresher.Start(ctx)
		logger.Info().Msg("provider config refresh started (30s interval)")
	} else {
		logger.Info().Msg("no postgres — using default providers only")
	}

	// --- Initialize Prometheus Metrics ---
	metricsRegistry := metrics.NewRegistry()
	metricsRegistry.RegisterDefault()
	metricsRegistry.BuildInfoMetric(config.Version, config.Commit, runtime.Version())

	authMetrics := metrics.NewAuthMetrics(metricsRegistry)
	quotaMetrics := metrics.NewQuotaMetrics(metricsRegistry)
	usageMetrics := metrics.NewUsageMetrics(metricsRegistry)
	recorderMetrics := metrics.NewRecorderMetrics(metricsRegistry)
	memoryMetrics := metrics.NewMemoryMetrics(metricsRegistry)

	logger.Info().
		Str("auth", "corp_proxy_auth_requests_total").
		Str("quota", "corp_proxy_quota_requests_total").
		Str("usage", "corp_proxy_usage_tokens_total").
		Msg("prometheus metrics registered")

	// Create metrics HTTP server on separate listener.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", metricsRegistry.Handler())
	metricsServer := &http.Server{
		Addr:         cfg.Server.MetricsAddr,
		Handler:      metricsMux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	go func() {
		logger.Info().Str("addr", cfg.Server.MetricsAddr).Msg("metrics server listening")
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal().Err(err).Msg("metrics server error")
		}
	}()

	// --- Initialize Auth ---
	var authMw *middleware.AuthMiddleware
	var revocationCache *redisstore.RevocationCache
	if pgPool != nil {
		keyStore := pgstore.NewKeyStore(pgPool)

		// Resolve the key-hash pepper from the secret store (or env).
		pepper := ""
		if cfg.Auth.HashPepperRef != "" {
			if secretStore != nil {
				if data, err := secretStore.Get(ctx, cfg.Auth.HashPepperRef); err == nil {
					pepper = string(data)
				}
			}
			// Also try direct env var as fallback.
			if pepper == "" {
				pepper = os.Getenv("PROXY_KEY_HASH_PEPPER")
			}
		}
		if pepper == "" {
			logger.Warn().Msg("no key-hash pepper configured; using SHA-256 (less secure)")
		}

		// Create revocation cache if Redis is available.
		if redisClient != nil && redisClient.IsConnected() {
			revocationCache = redisstore.NewRevocationCache(redisClient)
			authMw = middleware.NewAuthMiddleware(keyStore, pepper, revocationCache, authMetrics)
			logger.Info().Str("revocation", revocationCache.String()).Msg("auth middleware initialized with redis revocation cache")
		} else {
			authMw = middleware.NewAuthMiddleware(keyStore, pepper, nil, authMetrics)
			logger.Info().Msg("auth middleware initialized (no redis, revocation cache disabled)")
		}
	} else {
		logger.Fatal().Msg("auth middleware unavailable; refusing to start public proxy")
	}

	// --- Initialize Upstream Client ---
	upstreamClient := upstream.NewClient(upstream.Config{
		Timeout:         cfg.Server.RequestTimeout,
		MaxConnsPerHost: cfg.Upstream.MaxConnsPerHost,
		MaxIdleConns:    cfg.Upstream.MaxIdleConns,
	})
	logger.Info().Int("max_conns", cfg.Upstream.MaxConnsPerHost).Msg("upstream HTTP client initialized")

	// --- Initialize Request Rewriter ---
	requestRewriter := rewrite.NewRequestRewriter(providerRegistry)

	// --- Initialize IP Rate Limiter ---
	var ipLimiter *ratelimit.Middleware
	if cfg.RateLimit.RequestsPerMinute > 0 {
		store := ratelimit.NewMemoryStore()
		ipLimiter = ratelimit.NewMiddleware(store, cfg.RateLimit.RequestsPerMinute, 1*time.Minute)
		logger.Info().Int64("limit", cfg.RateLimit.RequestsPerMinute).Msg("IP rate limiter initialized")
	} else {
		logger.Info().Msg("IP rate limiter disabled")
	}

	// --- Initialize Usage Recorder ---
	costTable := usage.NewCostTable()
	usageRecorder := usage.NewRecorder(costTable, usageMetrics, recorderMetrics, cfg.Usage.QueueSize, logger)

	// Create usage store (nil if no Postgres).
	var usageStore *pgstore.UsageStore
	var subscriptionRates *subscriptioncost.Resolver
	if pgPool != nil {
		usageStore = pgstore.NewUsageStore(pgPool)
		rateStore := pgstore.NewSubscriptionRateStore(pgPool)
		subscriptionRates = subscriptioncost.NewResolver(rateStore)
		refreshSubscriptionRates := func() {
			if err := subscriptionRates.Refresh(ctx, time.Now()); err != nil {
				logger.Error().Err(err).Msg("subscription rate-card refresh failed")
			}
		}
		refreshSubscriptionRates()
		sweeperWg.Add(1)
		go func() {
			defer sweeperWg.Done()
			ticker := time.NewTicker(time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					refreshSubscriptionRates()
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	usageRecorder.SetSubscriptionRates(subscriptionRates)

	// Start a pool of usage event consumers in the background. A single writer
	// could not keep up under load and silently dropped billing events; the pool
	// drains the queue concurrently and records write success/failure metrics.
	{
		store := usageStore // capture for closure
		usageWorkers := cfg.Usage.Workers
		if usageWorkers <= 0 {
			usageWorkers = 4
		}
		for i := 0; i < usageWorkers; i++ {
			drainWg.Add(1)
			go func() {
				defer drainWg.Done()
				// Range exits when usageRecorder.Close() closes the channel at
				// shutdown, after every buffered event has been drained.
				for event := range usageRecorder.Events() {
					recorderMetrics.SetQueueDepth("usage", usageRecorder.QueueDepth())
					if store != nil {
						// consumerCtx() returns the root ctx normally and the
						// fresh drain ctx during shutdown so the final flush is
						// not cancelled by cancel().
						if err := store.InsertUsageEvent(consumerCtx(), event); err != nil {
							recorderMetrics.IncFailed("usage")
							logger.Error().Err(err).Str("request_id", event.RequestID).Msg("usage store write failed")
						} else {
							recorderMetrics.IncWritten("usage")
						}
					}
					logger.Debug().
						Str("request_id", event.RequestID).
						Str("model", event.Model).
						Int64("tokens", event.TotalTokens).
						Float64("cost", event.CostTotalCents).
						Msg("usage event")
				}
			}()
		}
		logger.Info().
			Int("workers", usageWorkers).
			Int("queue_size", cfg.Usage.QueueSize).
			Msg("usage recorder initialized")
	}

	// --- Initialize Audit Logger ---
	auditLogger := audit.NewLogger(1000)

	// Create audit store (nil if no Postgres).
	var auditStore *pgstore.AuditStore
	if pgPool != nil {
		auditStore = pgstore.NewAuditStore(pgPool)
	}

	// Start audit event consumer in background.
	// Attempts Postgres write when available; always logs.
	{
		store := auditStore // capture for closure
		drainWg.Add(1)
		go func() {
			defer drainWg.Done()
			// Range exits when auditLogger.Close() closes the channel at
			// shutdown, after every buffered event has been drained.
			for event := range auditLogger.Events() {
				if store != nil {
					if err := store.InsertAuditEvent(consumerCtx(), event); err != nil {
						logger.Error().Err(err).Str("request_id", event.RequestID).Msg("audit store write failed")
					}
				}
				logger.Debug().
					Str("action", event.Action).
					Str("actor", event.ActorID).
					Str("result", event.Result).
					Msg("audit event")
			}
		}()
	}
	logger.Info().Msg("audit logger initialized")

	// --- Initialize Session Submission Store ---
	var sessionSubmissionStore *pgstore.SessionSubmissionStore
	var piSessionLogStore *pgstore.PiSessionLogStore
	var memoryProjectStore *pgstore.MemoryProjectStore
	var sessionServices *sessionstorage.Services
	if pgPool != nil {
		var err error
		sessionServices, err = sessionstorage.Open(ctx, cfg.SessionArchive, pgPool, logger)
		if err != nil {
			logger.Fatal().Err(err).Msg("failed to open session archive storage")
		}
		defer func() { _ = sessionServices.Close() }()
		logger.Info().
			Str("objects", sessionServices.Objects.Location()).
			Str("raw_bodies", cfg.SessionArchive.RawBodies).
			Bool("capture_transcripts", cfg.SessionArchive.Enabled).
			Msg("session archive storage initialized")

		sessionSubmissionStore = pgstore.NewSessionSubmissionStore(pgPool)
		// Pi session exports follow the same placement as raw capture bodies.
		piSessionLogStore = pgstore.NewPiSessionLogStore(pgPool).WithObjects(
			sessionServices.Objects, cfg.SessionArchive.RawBodiesInObjects(), cfg.SessionArchive.RawBodiesInPostgres())
		memoryProjectStore = pgstore.NewMemoryProjectStore(pgPool)
		startPiSessionLogRetentionCleanup(ctx, &sweeperWg, piSessionLogStore, cfg.Capture.SessionLogRetentionDays, logger)
		logger.Info().Msg("session submission and Pi session log stores initialized")
	} else {
		logger.Warn().Msg("no postgres — session submission disabled")
	}

	// --- Initialize Conversation Capture Recorder ---
	var captureRecorder *capture.Recorder
	var captureGapRecorder *capture.GapRecorder
	if cfg.Capture.Enabled {
		switch cfg.Capture.Backend {
		case "postgres":
			if pgPool == nil {
				logger.Warn().Msg("conversation capture disabled: postgres backend requires postgres")
			} else {
				// Raw bodies go where PROXY_SESSION_RAW_BODIES says; each capture
				// is also appended to its conversation transcript.
				archiveMetrics := metrics.NewSessionArchiveMetrics(metricsRegistry)
				captureStore := sessionServices.CaptureStore(archiveMetrics)
				sessionServices.StartRepairWorker(ctx, archiveMetrics)
				// Gap markers record dropped/truncated captures so a session can be
				// audited for replayability; they use their own tiny queue so a full
				// payload queue cannot also block the markers for its own drops.
				gapRecorder := capture.NewGapRecorder(pgstore.NewCaptureGapStore(pgPool), cfg.Capture.QueueSize, recorderMetrics, logger)
				captureGapRecorder = gapRecorder
				captureRecorder = capture.NewRecorder(captureStore, gapRecorder, cfg.Capture.QueueSize, cfg.Capture.Workers, recorderMetrics, logger)
				// captureRecorder.Close(drainCtx) is invoked explicitly in the
				// ordered shutdown block below (not deferred) so its flush runs
				// before the root context is cancelled.
				startCaptureRetentionCleanup(ctx, &sweeperWg, sessionServices.Payloads, logger)
				logger.Info().
					Str("backend", cfg.Capture.Backend).
					Int("queue_size", cfg.Capture.QueueSize).
					Int("workers", cfg.Capture.Workers).
					Int64("max_payload_bytes", cfg.Capture.MaxPayloadBytes).
					Msg("conversation capture recorder initialized")
			}
		default:
			logger.Warn().Str("backend", cfg.Capture.Backend).Msg("conversation capture disabled: unsupported backend")
		}
	} else {
		logger.Info().Msg("conversation capture disabled")
	}

	// --- Initialize Project Memory (duffellib-backed) ---
	var (
		duffelClient       *duffel.Client
		memoryBuilder      *memory.Builder
		memoryHooks        *memory.HookChain
		memoryBootstrapper *memory.Bootstrapper
		distillationQueue  *memory.DistillationQueue
		compactor          *memory.Compactor
	)
	if cfg.Duffel.Root != "" {
		duffelClient = duffel.New(duffel.Config{
			Root:         cfg.Duffel.Root,
			EnableSearch: cfg.Duffel.EnableSearch,
			Collection:   cfg.Duffel.Collection,
		})
		memoryBuilder = memory.NewBuilder(duffelClient, memory.BundleConfig{
			BudgetTokens:     cfg.Memory.ContextBudgetTokens,
			CacheTTL:         cfg.Memory.ContextCacheTTL,
			SearchLimit:      cfg.Memory.SearchLimit,
			SearchMinScore:   cfg.Memory.SearchMinScore,
			SearchIntent:     cfg.Memory.SearchIntent,
			RecentLessons:    cfg.Memory.RecentLessons,
			MinContentTokens: cfg.Memory.MinBundleTokens,
		}, memoryMetrics, logger)

		memoryBootstrapper = memory.NewBootstrapper(duffelClient, logger)

		// Injection hook chain: global config toggle first, then operator
		// rules from the DB (admin /memory-rules API). Programmatic hooks can
		// be appended here as policies evolve.
		memoryHooks = memory.NewHookChain(0, logger, memory.NewGlobalToggleHook(func() bool {
			return cfg.Memory.InjectionEnabled
		}))
		if pgPool != nil {
			memoryHooks.Append(memory.NewRuleHook(pgstore.NewMemoryRuleStore(pgPool)))
		}
		logger.Info().Str("duffel_root", cfg.Duffel.Root).Msg("project memory initialized")

		// Internal LLM client for distillation/compaction.
		internalLLM := llmclient.New(providerRegistry, credentialStore, upstreamClient)

		if cfg.Memory.InsightGenerationEnabled && sessionSubmissionStore != nil && piSessionLogStore != nil && pgPool != nil {
			insightStore := pgstore.NewInsightStore(pgPool)
			insightClients := insights.NewClientPool()
			defer insightClients.Close()
			coordinator := insights.NewCoordinator(
				sessionSubmissionStore, piSessionLogStore, insightStore, insightClients, duffelClient, logger)
			queueCtx, queueCancel := context.WithTimeout(ctx, 10*time.Second)
			distillationQueue, err = memory.NewDistillationQueue(queueCtx, coordinator, sessionSubmissionStore, memory.DistillationQueueConfig{
				DatabaseURL:  cfg.Postgres.DSN,
				QueueName:    cfg.Memory.DistillationQueueName,
				Workers:      cfg.Memory.DistillationWorkers,
				PollInterval: cfg.Memory.DistillationPoll,
				ClaimTimeout: cfg.Memory.DistillationClaim,
				MaxAttempts:  cfg.Memory.DistillationAttempts,
				NightlyAtUTC: cfg.Memory.DistillationNightlyUTC,
				IdleAfter:    cfg.Memory.DistillerIdle,
			}, logger)
			queueCancel()
			if err != nil {
				logger.Fatal().Err(err).Msg("memory distillation queue unavailable")
			}
			defer distillationQueue.Close()
			reconcileCtx, reconcileCancel := context.WithTimeout(ctx, 30*time.Second)
			reconciled, reconcileErr := distillationQueue.ReconcileUndistilled(reconcileCtx)
			reconcileCancel()
			if reconcileErr != nil {
				logger.Fatal().Err(reconcileErr).Msg("memory distillation queue reconciliation failed")
			}
			sweeperWg.Add(1)
			go func() {
				defer sweeperWg.Done()
				distillationQueue.Start(ctx)
			}()
			logger.Info().
				Str("queue", cfg.Memory.DistillationQueueName).
				Str("nightly_at_utc", cfg.Memory.DistillationNightlyUTC).
				Dur("idle_after", cfg.Memory.DistillerIdle).
				Int("reconciled_sessions", reconciled).
				Msg("nightly external insight generation initialized")
		} else {
			logger.Info().Msg("external insight generation disabled (needs PROXY_INSIGHTS_ENABLED + postgres)")
		}

		compactorModel := cfg.Memory.CompactorModel
		if compactorModel == "" {
			compactorModel = cfg.Memory.DistillerModel
		}
		if cfg.Memory.CompactionEnabled && compactorModel != "" {
			compactor = memory.NewCompactor(duffelClient, internalLLM, memory.CompactorConfig{
				Model:           compactorModel,
				Interval:        cfg.Memory.CompactionInterval,
				ThresholdTokens: cfg.Memory.CompactionThreshold,
				MaxTokens:       cfg.Memory.CompactionMaxTokens,
			}, logger)
			var compactionQueue *memory.CompactionQueue
			if cfg.Memory.CompactionQueue {
				queueCtx, queueCancel := context.WithTimeout(ctx, 10*time.Second)
				compactionQueue, err = memory.NewCompactionQueue(queueCtx, compactor, memory.CompactionQueueConfig{
					DatabaseURL:    cfg.Postgres.DSN,
					QueueName:      cfg.Memory.CompactionQueueName,
					Workers:        cfg.Memory.CompactionWorkers,
					PollInterval:   cfg.Memory.CompactionPoll,
					ClaimTimeout:   cfg.Memory.CompactionClaim,
					DedupInterval:  cfg.Memory.CompactionInterval,
					PeakWindowsUTC: cfg.Memory.CompactionPeakUTC,
				}, logger)
				queueCancel()
				if err != nil {
					logger.Error().Err(err).Msg("memory compaction queue unavailable; compactor disabled to avoid peak-hour LLM usage")
				} else {
					defer compactionQueue.Close()
					compactor.SetQueue(compactionQueue)
					sweeperWg.Add(1)
					go func() {
						defer sweeperWg.Done()
						compactionQueue.Start(ctx)
					}()
				}
			}
			if !cfg.Memory.CompactionQueue || compactionQueue != nil {
				sweeperWg.Add(1)
				go func() {
					defer sweeperWg.Done()
					compactor.Start(ctx)
				}()
				logger.Info().
					Str("model", compactorModel).
					Bool("queued", compactionQueue != nil).
					Str("queue", cfg.Memory.CompactionQueueName).
					Str("peak_windows_utc", cfg.Memory.CompactionPeakUTC).
					Msg("memory compactor initialized for hook/UI-driven runs")
			}
		} else {
			logger.Info().Msg("memory compactor disabled (needs a compactor/distiller model)")
		}
	} else if cfg.Duffel.URL != "" {
		logger.Warn().Str("duffel_url", cfg.Duffel.URL).Msg("PROXY_DUFFEL_URL is deprecated and ignored; set PROXY_DUFFEL_ROOT to enable project memory")
	} else {
		logger.Info().Msg("project memory disabled (PROXY_DUFFEL_ROOT not set)")
	}

	// --- Initialize metadata-only Hooks ---
	var hookSinks []hooks.Sink
	if cfg.Hooks.EventsFile != "" {
		fileSink, err := hooks.NewJSONLFileSink(cfg.Hooks.EventsFile)
		if err != nil {
			logger.Fatal().Err(err).Str("events_file", cfg.Hooks.EventsFile).Msg("initialize embedded hook event sink")
		}
		hookSinks = append(hookSinks, fileSink)
	}
	hookManager := hooks.NewManager(hooks.Config{
		WebhookURL:    cfg.Hooks.WebhookURL,
		WebhookSecret: cfg.Hooks.WebhookSecret,
		QueueSize:     cfg.Hooks.QueueSize,
		Timeout:       cfg.Hooks.Timeout,
		Sinks:         hookSinks,
	}, logger)
	if distillationQueue != nil {
		hookManager.Append(memory.NewSessionUploadedHook(distillationQueue, sessionSubmissionStore, logger))
		logger.Info().Msg("nightly memory distillation enqueue hook registered")
	}
	if hookManager.HasSinks() {
		logger.Info().Str("events_file", cfg.Hooks.EventsFile).Msg("embedded metadata hook event sink initialized")
	}
	if hookManager.HasWebhook() {
		logger.Info().Str("webhook_url", cfg.Hooks.WebhookURL).Msg("external metadata hook webhook initialized")
	}

	// --- Initialize DB-Managed Hook Subscriptions ---
	// Off by default: blocking subscriptions add latency to every matching
	// request, including existing CLI traffic, so this is enabled deliberately
	// rather than by upgrading.
	var hookDeliveries *hooks.AsyncDeliveryRecorder
	if cfg.Hooks.SubscriptionsEnabled && pgPool != nil {
		// A nil encryptor means hook secrets cannot be stored, so subscriptions
		// are registered unsigned. That is only appropriate on a trusted
		// network, which is why it is called out here rather than passed
		// silently.
		var hookCipher *secrets.Encryptor
		if cfg.Secrets.CredentialEncryptionKey != "" {
			var err error
			if hookCipher, err = secrets.NewEncryptorFromKeyMaterial(cfg.Secrets.CredentialEncryptionKey); err != nil {
				logger.Fatal().Err(err).Msg("hook secret encryption key is invalid")
			}
		} else {
			logger.Warn().Msg("hook subscriptions enabled without PROXY_CREDENTIAL_ENCRYPTION_KEY: deliveries will be unsigned")
		}
		// Passing a typed-nil *secrets.Encryptor would produce a non-nil
		// interface the store would then call, so the nil case passes a
		// literal nil instead.
		var hookStore *pgstore.HookSubscriptionStore
		if hookCipher != nil {
			hookStore = pgstore.NewHookSubscriptionStore(pgPool, hookCipher)
		} else {
			hookStore = pgstore.NewHookSubscriptionStore(pgPool, nil)
		}
		registry, err := hooks.NewRegistry(ctx, hookStore.LoadActive, cfg.Hooks.RefreshInterval, logger)
		if err != nil {
			logger.Fatal().Err(err).Msg("load hook subscriptions")
		}
		registry.Start(ctx, &sweeperWg)
		hookDeliveries = hooks.NewAsyncDeliveryRecorder(func(ctx context.Context, rec hooks.DeliveryRecord) error {
			return hookStore.InsertDelivery(ctx, &pgstore.HookDeliveryRecord{
				SubscriptionID: rec.SubscriptionID,
				EventID:        rec.EventID,
				EventType:      string(rec.EventType),
				TenantID:       rec.TenantID,
				Status:         rec.Status,
				HTTPStatus:     rec.HTTPStatus,
				LatencyMS:      int(rec.Latency.Milliseconds()),
				Error:          rec.Error,
				Decision:       marshalHookDecision(rec.Decision),
			})
		}, cfg.Hooks.QueueSize, logger)

		hookManager.SetSubscriptions(registry)
		hookManager.SetDeliveryRecorder(hookDeliveries)
		hookManager.SetBlockingBudget(cfg.Hooks.BlockingBudget)
		hookManager.SetAllowPrivateHookEgress(cfg.Hooks.AllowPrivateEgress)
		startHookDeliveryCleanup(ctx, &sweeperWg, hookStore, cfg.Hooks.DeliveryRetentionDays, logger)
		logger.Info().
			Int("subscriptions", len(registry.Subscriptions())).
			Dur("blocking_budget", cfg.Hooks.BlockingBudget).
			Msg("database-managed hook subscriptions initialized")
	}

	// --- Initialize Quota Enforcer ---
	quotaManager := quota.NewManager()
	if pgPool != nil {
		quotaPolicyStore := pgstore.NewQuotaPolicyStore(pgPool)
		lastQuotaPolicySummary := ""
		refreshQuotaPolicies := func() {
			records, err := quotaPolicyStore.ListAll(ctx)
			if err != nil {
				logger.Error().Err(err).Msg("quota policy refresh failed")
				return
			}
			policies := make([]*quota.Policy, 0, len(records))
			for _, record := range records {
				policy, err := record.ToPolicy()
				if err != nil {
					logger.Warn().Err(err).Str("policy_id", record.ID).Msg("skipping invalid quota policy")
					continue
				}
				policies = append(policies, policy)
			}
			quotaManager.SetPolicies(policies)
			summary := fmt.Sprintf("count=%d", len(policies))
			if summary != lastQuotaPolicySummary {
				logger.Info().Int("count", len(policies)).Msg("quota policies refreshed")
				lastQuotaPolicySummary = summary
			} else {
				logger.Debug().Int("count", len(policies)).Msg("quota policies refreshed")
			}
		}
		refreshQuotaPolicies()
		sweeperWg.Add(1)
		go func() {
			defer sweeperWg.Done()
			ticker := time.NewTicker(cfg.Providers.RefreshInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					refreshQuotaPolicies()
				case <-ctx.Done():
					return
				}
			}
		}()
	} else {
		logger.Info().Msg("no postgres — using default quota policies only")
	}
	quotaEnforcer := quota.NewEnforcer(quotaManager, redisClient, quotaMetrics)
	quotaEnforcer.SetSpendSettlementStore(pgstore.NewQuotaSpendSettlementStore(pgPool))
	logger.Info().Msg("quota enforcer initialized")

	// --- Initialize Private Web Portal ---
	modelAccessStore := pgstore.NewTenantModelSettingsStore(pgPool)
	webConsole, err := setupWebConsole(ctx, cfg, webConsoleInputs{
		Pool:             pgPool,
		RevocationCache:  revocationCache,
		Registry:         providerRegistry,
		ModelAccess:      modelAccessStore,
		ModelGuards:      cfg.ModelGuards,
		QuotaManager:     quotaManager,
		QuotaCounter:     redisClient,
		CacheTrendReader: usageStore,
		MemoryBuilder:    memoryBuilder,
		Duffel:           duffelClient,
		Projects:         memoryProjectStore,
		Hooks:            hookManager,
	}, logger)
	if err != nil {
		logger.Fatal().Err(err).Msg("web console initialization failed")
	}
	if webConsole != nil {
		defer webConsole.Close()
		startWebConsoleCleanup(ctx, &sweeperWg, webConsole, pgPool, cfg.Web.AttachmentPendingTTL, logger)
	}

	// --- Build Public Router ---
	deps := &httpapi.Dependencies{
		ProviderRegistry:       providerRegistry,
		AuthMiddleware:         authMw,
		UpstreamClient:         upstreamClient,
		RequestRewriter:        requestRewriter,
		IPRateLimiter:          ipLimiter,
		CredentialStore:        credentialStore,
		QuotaEnforcer:          quotaEnforcer,
		QuotaManager:           quotaManager,
		QuotaCounter:           redisClient,
		CacheTrendReader:       usageStore,
		UsageRecorder:          usageRecorder,
		CostTable:              costTable,
		AuditLogger:            auditLogger,
		CaptureRecorder:        captureRecorder,
		HookManager:            hookManager,
		SessionSubmissionStore: sessionSubmissionStore,
		ExtensionErrorStore:    pgstore.NewExtensionErrorStore(pgPool),
		PiSessionLogStore:      piSessionLogStore,
		MemoryProjectStore:     memoryProjectStore,
		MemoryBuilder:          memoryBuilder,
		MemoryHooks:            memoryHooks,
		MemoryBootstrapper:     memoryBootstrapper,
		ModelAccessStore:       modelAccessStore,
		SubscriptionRates:      subscriptionRates,
		MemoryContextConfig: memorycontext.Config{
			DefaultBudget: cfg.Memory.ContextBudgetTokens,
		},
		DuffelClient:        duffelClient,
		ObservationMaxBytes: cfg.Memory.ObservationMaxBytes,
		WebConsole:          webConsoleFactory(webConsole),
		CaptureConfig: dataplane.CaptureSettings{
			MaxPayloadBytes: cfg.Capture.MaxPayloadBytes,
			RetentionDays:   cfg.Capture.RetentionDays,
		},
		Logger: logger,
		ReadyCheck: func() error {
			// Check Postgres.
			if pgPool != nil {
				if err := pgPool.Health(ctx); err != nil {
					return fmt.Errorf("postgres: %w", err)
				}
			}
			// Check Redis.
			if redisClient.IsConnected() {
				if err := redisClient.Health(ctx); err != nil {
					return fmt.Errorf("redis: %w", err)
				}
			}
			// Check secret store.
			if secretStore != nil {
				if err := secretStore.Health(ctx); err != nil {
					return fmt.Errorf("secrets: %w", err)
				}
			}
			return nil
		},
	}

	router := httpapi.NewPublicRouter(httpapi.RouterConfig{
		CodexQuotaDisabled: cfg.Quota.CodexDisabled,
		MaxBodyBytes:       cfg.Server.MaxBodyBytes,
		CORS:               middleware.DefaultCORSConfig(),
		UseSecurityHdr:     true,
		RequestTimeout:     cfg.Server.RequestTimeout,
		ModelGuards:        cfg.ModelGuards,
	}, deps)

	// Create public server.
	publicServer := &http.Server{
		Addr:         cfg.Server.PublicAddr,
		Handler:      router,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}

	// All servers for graceful shutdown.
	servers := []*http.Server{publicServer, metricsServer}

	// Start public server.
	go func() {
		logger.Info().Str("addr", cfg.Server.PublicAddr).Msg("public server listening")
		if err := publicServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatal().Err(err).Msg("public server error")
		}
	}()

	// Wait for shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	sig := <-sigCh

	logger.Info().Str("signal", sig.String()).Msg("shutting down")

	// Ordered shutdown. The phases below are sequenced deliberately so no
	// buffered billing/audit/capture/hook event is lost and no sweeper write
	// races a closed pool. Do NOT reorder.

	// Phase 1 — STOP INTAKE. srv.Shutdown drains in-flight HTTP requests and
	// refuses new ones. Once it returns, no new handler can call
	// usageRecorder.Record / auditLogger.Log / captureRecorder.Submit /
	// hookManager.EmitAsync. This uses the elapsing shutdownCtx for HTTP only.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer shutdownCancel()
	for _, srv := range servers {
		logger.Info().Str("addr", srv.Addr).Msg("shutting down server")
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error().Err(err).Str("addr", srv.Addr).Msg("server shutdown error")
		}
	}

	// Phase 2 — INSTALL A FRESH DRAIN CONTEXT. The drain-group consumers write
	// to Postgres using consumerCtx(); point it at a fresh context derived from
	// context.Background() (NOT the root ctx, NOT the partially-elapsed
	// shutdownCtx) so the final flush has a full time budget and is not
	// cancelled by Phase 4's cancel().
	drainTimeoutCtx, drainTimeoutCancel := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
	defer drainTimeoutCancel()
	drainCtx.Store(&drainTimeoutCtx)

	// Phase 3 — CLOSE INTAKE CHANNELS + WAIT FOR THE DRAIN GROUP. Each Close()
	// sets a stopped flag before closing its channel so any late, detached
	// caller does a guarded no-op send instead of panicking. Closing the
	// channels lets the consumers' range loops drain buffered events and exit;
	// drainWg.Wait() blocks until those writes complete under drainTimeoutCtx.
	usageRecorder.Close()
	auditLogger.Close()
	hookManager.Close() // blocks until its own run() goroutine drains the queue
	hookDeliveries.Close()
	drainWg.Wait()
	// captureRecorder owns its own worker WaitGroup; Close(drainCtx) flushes the
	// queue and waits for the workers, bounded by the drain context.
	if captureRecorder != nil {
		captureRecorder.Close(drainTimeoutCtx)
	}
	if captureGapRecorder != nil {
		captureGapRecorder.Close(drainTimeoutCtx)
	}
	logger.Info().Msg("async consumers drained")

	// Phase 4 — CANCEL ROOT CTX + WAIT FOR SWEEPERS. The workers/sweepers
	// (distillation queue, compactor, quota-policy refresh, and capture-retention
	// cleanup) only stop on ctx.Done(), so they are stopped here, AFTER the drain
	// group has flushed.
	// providerRefresher.Stop() blocks until its goroutine exits.
	if providerRefresher != nil {
		providerRefresher.Stop()
		logger.Info().Msg("provider config refresh stopped")
	}
	cancel()
	sweeperWg.Wait()
	logger.Info().Msg("background sweepers stopped")

	// Phase 5 — CLOSE BACKING STORES LAST. The deferred pgPool.Close() and
	// redisClient.Close() run on return, after Phases 3 and 4 have completed
	// synchronously above, so no late write can hit a closed pool.
	upstreamClient.CloseIdleConnections()
	logger.Info().Msg("proxy server stopped")
	fmt.Println("proxy server stopped gracefully")
}

func startCaptureRetentionCleanup(ctx context.Context, wg *sync.WaitGroup, store *pgstore.ConversationPayloadStore, logger zerolog.Logger) {
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		deleted, err := store.DeleteExpired(cleanupCtx, time.Now())
		if err != nil {
			logger.Error().Err(err).Msg("conversation capture retention cleanup failed")
			return
		}
		if deleted > 0 {
			logger.Info().Int64("deleted", deleted).Msg("conversation capture retention cleanup complete")
		}
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		cleanup()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				cleanup()
			case <-ctx.Done():
				return
			}
		}
	}()
}

func startPiSessionLogRetentionCleanup(ctx context.Context, wg *sync.WaitGroup, store *pgstore.PiSessionLogStore, retentionDays int, logger zerolog.Logger) {
	if store == nil || retentionDays <= 0 {
		logger.Warn().Int("retention_days", retentionDays).Msg("Pi session log retention cleanup disabled")
		return
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
		deleted, err := store.DeleteOlderThan(cleanupCtx, cutoff)
		if err != nil {
			logger.Error().Err(err).Msg("Pi session log retention cleanup failed")
			return
		}
		if deleted > 0 {
			logger.Info().Int64("deleted", deleted).Int("retention_days", retentionDays).Msg("Pi session log retention cleanup complete")
		}
	}
	cleanup()
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cleanup()
			}
		}
	}()
}
