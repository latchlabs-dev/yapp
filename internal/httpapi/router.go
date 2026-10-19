// Package httpapi provides the public HTTP router and operational endpoints.
package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/audit"
	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/httpapi/anthropic"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/extensionerrors"
	"github.com/earlgrey/piproxy/internal/httpapi/extensionversion"
	"github.com/earlgrey/piproxy/internal/httpapi/generic"
	"github.com/earlgrey/piproxy/internal/httpapi/memorycontext"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/httpapi/observations"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/httpapi/projectresolve"
	"github.com/earlgrey/piproxy/internal/httpapi/proxymodels"
	"github.com/earlgrey/piproxy/internal/httpapi/quotausage"
	"github.com/earlgrey/piproxy/internal/httpapi/sessionlogs"
	"github.com/earlgrey/piproxy/internal/httpapi/sessionsubmit"
	"github.com/earlgrey/piproxy/internal/memory"
	"github.com/earlgrey/piproxy/internal/modelaccess"
	"github.com/earlgrey/piproxy/internal/modelguard"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/ratelimit"
	"github.com/earlgrey/piproxy/internal/rewrite"
	"github.com/earlgrey/piproxy/internal/secrets"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	"github.com/earlgrey/piproxy/internal/subscriptioncost"
	"github.com/earlgrey/piproxy/internal/upstream"
	"github.com/earlgrey/piproxy/internal/usage"
)

// RouterConfig holds configuration for the public HTTP router.
type RouterConfig struct {
	CodexQuotaDisabled bool
	MaxBodyBytes       int64
	CORS               middleware.CORSConfig
	UseSecurityHdr     bool
	RequestTimeout     time.Duration
	// ModelGuards are static stop-with-custom-error rules applied by every
	// provider handler before routing/quota and mirrored in the advertised
	// model catalog. Zero value = no guards.
	ModelGuards modelguard.Rules
}

// Dependencies holds all the dependencies needed by the router.
type Dependencies struct {
	ProviderRegistry       *providers.Registry
	AuthMiddleware         *middleware.AuthMiddleware
	UpstreamClient         *upstream.Client
	RequestRewriter        *rewrite.RequestRewriter
	IPRateLimiter          *ratelimit.Middleware
	CredentialStore        *secrets.CredentialStore
	QuotaEnforcer          *quota.Enforcer
	QuotaManager           *quota.Manager
	QuotaCounter           quotausage.CounterReader
	CacheTrendReader       usage.CacheTrendReader
	UsageRecorder          *usage.Recorder
	CostTable              *usage.KnownCostTable
	AuditLogger            *audit.Logger
	CaptureRecorder        *capture.Recorder
	CaptureConfig          dataplane.CaptureSettings
	HookManager            *hooks.Manager
	SessionSubmissionStore *pgstore.SessionSubmissionStore
	ExtensionErrorStore    *pgstore.ExtensionErrorStore
	PiSessionLogStore      *pgstore.PiSessionLogStore
	MemoryProjectStore     *pgstore.MemoryProjectStore
	MemoryBuilder          *memory.Builder      // nil = context injection disabled
	MemoryHooks            *memory.HookChain    // injection policy chain
	MemoryBootstrapper     *memory.Bootstrapper // scaffolds new project folders
	MemoryContextConfig    memorycontext.Config
	ModelAccessStore       modelaccess.Store
	SubscriptionRates      subscriptioncost.RateResolver
	DuffelClient           *duffel.Client // nil = observations disabled
	ObservationMaxBytes    int
	// WebConsole builds the /web/v1 handler. It is a factory rather than a
	// handler because the console delegates turns to the protocol handlers
	// below, which are constructed here. nil disables the console.
	WebConsole func(*openai.ChatHandler, *anthropic.MessagesHandler) http.Handler
	Logger     zerolog.Logger
	ReadyCheck func() error // nil = always ready
}

// NewPublicRouter creates the public data-plane HTTP router using chi.
func NewPublicRouter(cfg RouterConfig, deps *Dependencies) http.Handler {
	r := chi.NewRouter()

	// Global middleware (applied to all routes). Recover is registered FIRST so
	// it is the outermost wrapper and can catch panics in every other middleware
	// (including RequestID/RequestLogging) and in handlers. It degrades
	// gracefully for the rare panic before RequestID runs (GetRequestID -> "",
	// GetRequestLogger -> Nop).
	r.Use(middleware.Recover)
	r.Use(middleware.RequestID)
	r.Use(middleware.SecurityHeaders)
	r.Use(middleware.CORS(cfg.CORS))
	if deps != nil {
		r.Use(middleware.RequestLogging(deps.Logger))
	}
	if cfg.RequestTimeout > 0 {
		r.Use(middleware.Timeout(cfg.RequestTimeout))
	}

	// Public endpoints (no auth).
	r.Group(func(r chi.Router) {
		r.Get("/healthz", HealthzHandler)
		r.Get("/readyz", NewReadyzHandler(depsReadyCheck(deps)))
	})

	// Protocol handlers are built once and shared. The web console delegates
	// its turns to these same instances, so console traffic goes through the
	// identical quota, usage, capture, cost, model-access, audit and hook path
	// as CLI traffic instead of a parallel implementation.
	var (
		chatHandler        *openai.ChatHandler
		msgHandler         *anthropic.MessagesHandler
		passthroughHandler *generic.PassthroughHandler
	)
	if deps != nil {
		svcs := dataplane.Services{
			Credentials:        deps.CredentialStore,
			Usage:              deps.UsageRecorder,
			CostTable:          deps.CostTable,
			Audit:              deps.AuditLogger,
			Quota:              deps.QuotaEnforcer,
			CodexQuotaDisabled: cfg.CodexQuotaDisabled,
			Capture:            deps.CaptureRecorder,
			CaptureConfig:      deps.CaptureConfig,
			Hooks:              deps.HookManager,
			ModelAccess:        deps.ModelAccessStore,
			ModelGuards:        cfg.ModelGuards,
			SubscriptionRates:  deps.SubscriptionRates,
		}
		chatHandler = openai.NewChatHandler(
			deps.UpstreamClient, deps.RequestRewriter,
			deps.ProviderRegistry, svcs,
		)
		msgHandler = anthropic.NewMessagesHandler(
			deps.UpstreamClient, deps.RequestRewriter,
			deps.ProviderRegistry, svcs,
		)
		passthroughHandler = generic.NewPassthroughHandler(
			deps.UpstreamClient, deps.RequestRewriter,
			deps.ProviderRegistry, svcs,
		)
	}

	// Public web console. It gets its own group because it authenticates by
	// session cookie: the API-key AuthMiddleware in the group below would
	// reject every console request for carrying no key. Its session middleware
	// produces an equivalent auth context, so everything downstream is the same.
	if deps != nil && deps.WebConsole != nil {
		r.Group(func(r chi.Router) {
			if deps.IPRateLimiter != nil {
				r.Use(deps.IPRateLimiter.Wrap)
			}
			r.Use(middleware.BodySize(cfg.MaxBodyBytes))
			r.Mount("/web/v1", deps.WebConsole(chatHandler, msgHandler))
		})
	}

	// Provider API routes (authenticated).
	r.Group(func(r chi.Router) {
		if deps != nil && deps.IPRateLimiter != nil {
			r.Use(deps.IPRateLimiter.Wrap)
		}
		r.Use(middleware.BodySize(cfg.MaxBodyBytes))
		if deps != nil && deps.AuthMiddleware != nil {
			r.Use(deps.AuthMiddleware.Authenticate)
		}

		if deps != nil {

			// Apply scope checks only when auth is configured.
			if deps.AuthMiddleware != nil {
				r.With(middleware.RequireAnyScope(auth.ScopeLLMInvoke, auth.ScopeLLMInvokeOpenAI)).
					Post("/openai/v1/chat/completions", chatHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke, auth.ScopeLLMInvokeOpenAI)).
					Get("/openai/v1/models", NotImplementedHandler("openai/models"))
				r.With(middleware.RequireAnyScope(auth.ScopeLLMInvoke, auth.ScopeLLMInvokeDeepSeek)).
					Post("/deepseek/v1/chat/completions", chatHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke, auth.ScopeLLMInvokeDeepSeek)).
					Get("/deepseek/v1/models", NotImplementedHandler("deepseek/models"))
				r.With(middleware.RequireAnyScope(auth.ScopeLLMInvoke, auth.ScopeLLMInvokeAnthropic)).
					Post("/anthropic/v1/messages", msgHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke, auth.ScopeLLMInvokeAnthropic)).
					Get("/anthropic/v1/models", NotImplementedHandler("anthropic/models"))

				// Generic passthrough routes — any provider whose API
				// protocol the proxy does not have a dedicated handler for
				// is served by the passthrough handler. Paths are matched
				// against provider PublicBasePath entries in the registry.
				//
				// Handled protocols: mistral-conversations, google-generative-ai,
				// google-vertex, azure-openai-responses, openai-codex-responses,
				// bedrock-converse-stream.
				r.With(middleware.RequireAnyScope(auth.ScopeLLMInvoke)).
					Post("/mistral/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke)).
					Get("/mistral/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMInvoke)).
					Post("/google/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke)).
					Get("/google/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMInvoke)).
					Post("/vertex/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke)).
					Get("/vertex/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMInvoke)).
					Post("/azure-openai/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke)).
					Get("/azure-openai/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMInvoke)).
					Post("/codex/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke)).
					Get("/codex/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMInvoke)).
					Post("/bedrock/*", passthroughHandler.Handle)
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke)).
					Get("/bedrock/*", passthroughHandler.Handle)

				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke)).
					Get("/proxy/v1/models", proxymodels.NewGuardedTenantAwareHandler(deps.ProviderRegistry, deps.ModelAccessStore, cfg.ModelGuards))
				// Any authenticated key may inspect only its own applicable
				// API-key, user, and tenant quota balances.
				r.Get("/proxy/v1/quota", quotausage.NewHandler(
					deps.QuotaManager,
					deps.QuotaCounter,
					deps.CacheTrendReader,
				))

				// Session submission from corp-proxy extension.
				submitHandler := sessionSubmitHandler(deps)
				r.With(middleware.RequireAnyScope(auth.ScopeSessionSubmit)).
					Post("/proxy/v1/session-submit", submitHandler)
				// Error reports deliberately require authentication but no extra
				// scope: this path must remain available when a scoped operation is
				// precisely what failed.
				r.Post("/proxy/v1/extension-errors", extensionErrorsHandler(deps))
				r.Post("/proxy/v1/projects/resolve", projectResolveHandler(deps))
				if deps.PiSessionLogStore != nil {
					sessionLogHandler := sessionlogs.NewHandler(deps.PiSessionLogStore, deps.MemoryProjectStore, deps.Logger)
					r.With(middleware.RequireAnyScope(auth.ScopeSessionLogsWrite)).
						Post("/proxy/v1/session-logs/status", sessionLogHandler.Status)
					r.With(middleware.RequireAnyScope(auth.ScopeSessionLogsWrite)).
						Put("/proxy/v1/session-logs/{sessionID}", sessionLogHandler.Upload)
				} else {
					r.Post("/proxy/v1/session-logs/status", sessionLogsUnavailableHandler)
					r.Put("/proxy/v1/session-logs/{sessionID}", sessionLogsUnavailableHandler)
				}

				// Extension auto-update endpoints.
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke)).
					Get("/proxy/v1/extension-version", extensionversion.NewVersionHandler())
				r.With(middleware.RequireAnyScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke)).
					Get("/proxy/v1/extension-download", extensionversion.NewDownloadHandler())

				// Project memory: context bundles + agent observations.
				r.With(middleware.RequireAnyScope(auth.ScopeMemoryContextRead)).
					Get("/proxy/v1/context", memoryContextHandler(deps))
				r.With(middleware.RequireAnyScope(auth.ScopeMemoryContextRead)).
					Post("/proxy/v1/context", memoryContextHandler(deps))
				r.With(middleware.RequireAnyScope(auth.ScopeMemoryObserve)).
					Post("/proxy/v1/observations", observationsHandler(deps))
			} else {
				r.Post("/openai/v1/chat/completions", AuthUnavailableHandler)
				r.Get("/openai/v1/models", AuthUnavailableHandler)
				r.Post("/deepseek/v1/chat/completions", AuthUnavailableHandler)
				r.Get("/deepseek/v1/models", AuthUnavailableHandler)
				r.Post("/anthropic/v1/messages", AuthUnavailableHandler)
				r.Get("/anthropic/v1/models", AuthUnavailableHandler)
				r.Get("/proxy/v1/models", AuthUnavailableHandler)
				r.Get("/proxy/v1/quota", AuthUnavailableHandler)
				r.Post("/proxy/v1/session-submit", AuthUnavailableHandler)
				r.Post("/proxy/v1/extension-errors", AuthUnavailableHandler)
				r.Post("/proxy/v1/projects/resolve", AuthUnavailableHandler)
				r.Post("/proxy/v1/session-logs/status", AuthUnavailableHandler)
				r.Put("/proxy/v1/session-logs/{sessionID}", AuthUnavailableHandler)
				r.Get("/proxy/v1/extension-version", AuthUnavailableHandler)
				r.Get("/proxy/v1/extension-download", AuthUnavailableHandler)
				r.Get("/proxy/v1/context", AuthUnavailableHandler)
				r.Post("/proxy/v1/context", AuthUnavailableHandler)
				r.Post("/proxy/v1/observations", AuthUnavailableHandler)
			}
		} else {
			r.Post("/openai/v1/chat/completions", NotImplementedHandler("openai/chat"))
			r.Get("/openai/v1/models", NotImplementedHandler("openai/models"))
			r.Post("/deepseek/v1/chat/completions", NotImplementedHandler("deepseek/chat"))
			r.Get("/deepseek/v1/models", NotImplementedHandler("deepseek/models"))
			r.Post("/anthropic/v1/messages", NotImplementedHandler("anthropic/messages"))
			r.Get("/anthropic/v1/models", NotImplementedHandler("anthropic/models"))
			if deps != nil {
				r.Get("/proxy/v1/models", proxymodels.NewHandler(deps.ProviderRegistry))
			} else {
				r.Get("/proxy/v1/models", proxymodels.NewHandler())
			}
		}
	})
	return r
}

func sessionLogsUnavailableHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"error": {"type": "unavailable", "message": "Pi session log storage not configured"},
	})
}

// sessionSubmitHandler returns the session submission handler or a 503 fallback.
func sessionSubmitHandler(deps *Dependencies) http.HandlerFunc {
	if deps != nil && deps.SessionSubmissionStore != nil {
		return sessionsubmit.NewHandler(deps.SessionSubmissionStore, deps.MemoryProjectStore, deps.HookManager, deps.Logger)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]map[string]string{
			"error": {"type": "unavailable", "message": "session submission not configured"},
		})
	}
}

func extensionErrorsHandler(deps *Dependencies) http.HandlerFunc {
	if deps != nil && deps.ExtensionErrorStore != nil {
		return extensionerrors.NewHandler(deps.ExtensionErrorStore, deps.Logger)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]map[string]string{
			"error": {"type": "unavailable", "message": "extension error monitoring not configured"},
		})
	}
}

func projectResolveHandler(deps *Dependencies) http.HandlerFunc {
	if deps != nil && deps.MemoryProjectStore != nil {
		return projectresolve.NewHandler(deps.MemoryProjectStore)
	}
	return memoryUnavailableHandler("project resolution not configured")
}

// memoryContextHandler returns the context bundle handler or a 503 fallback.
func memoryContextHandler(deps *Dependencies) http.HandlerFunc {
	if deps != nil && deps.MemoryBuilder != nil {
		return memorycontext.NewHandler(deps.MemoryBuilder, deps.MemoryHooks, deps.MemoryBootstrapper, deps.MemoryProjectStore, deps.MemoryContextConfig, deps.Logger)
	}
	return memoryUnavailableHandler("context injection not configured")
}

// observationsHandler returns the observation handler or a 503 fallback.
func observationsHandler(deps *Dependencies) http.HandlerFunc {
	if deps != nil && deps.DuffelClient != nil {
		return observations.NewHandler(deps.DuffelClient, deps.MemoryProjectStore, deps.ObservationMaxBytes, deps.Logger)
	}
	return memoryUnavailableHandler("observations not configured")
}

func memoryUnavailableHandler(message string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]map[string]string{
			"error": {"type": "unavailable", "message": message},
		})
	}
}

// depsReadyCheck returns a ReadyCheck that verifies all configured dependencies.
func depsReadyCheck(deps *Dependencies) func() error {
	if deps == nil {
		return nil
	}
	return deps.ReadyCheck
}

// NewTestRouter creates a router without auth for testing.
func NewTestRouter() http.Handler {
	return NewPublicRouter(RouterConfig{
		MaxBodyBytes:   1048576,
		CORS:           middleware.DefaultCORSConfig(),
		UseSecurityHdr: true,
	}, nil)
}

// NotImplementedHandler returns a 501 for unimplemented endpoints.
func NotImplementedHandler(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotImplemented)
		_ = json.NewEncoder(w).Encode(map[string]map[string]string{
			"error": {
				"type":    "not_implemented",
				"message": name + " endpoint not yet implemented",
			},
		})
	}
}

// AuthUnavailableHandler fails closed when provider routes are built without
// an authentication backend.
func AuthUnavailableHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"error": {
			"type":    "auth_unavailable",
			"message": "authentication is unavailable",
		},
	})
}
