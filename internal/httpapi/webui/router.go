// Package webui serves the Tailscale-only portal API under /web/v1. Public
// nginx denies these routes even when the shared backend mounts them.
//
// Requests authenticate with a cookie: either an encrypted existing API key or
// an OIDC session. Key cookies retain their key's quota/usage identity. Traffic is
// then delegated in-process to the existing protocol handlers, which keeps
// quota, usage, capture, cost, model access, audit and hooks identical to CLI
// traffic instead of reimplementing them.
package webui

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/httpapi/anthropic"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/httpapi/openai"
	"github.com/earlgrey/piproxy/internal/httpapi/proxymodels"
	"github.com/earlgrey/piproxy/internal/httpapi/quotausage"
	"github.com/earlgrey/piproxy/internal/memory"
	"github.com/earlgrey/piproxy/internal/modelaccess"
	"github.com/earlgrey/piproxy/internal/modelguard"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/ratelimit"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	"github.com/earlgrey/piproxy/internal/usage"
	"github.com/earlgrey/piproxy/internal/webauth"
)

// Config carries the web console settings the handlers need at request time.
type Config struct {
	Enabled            bool
	BaseURL            string
	ProviderName       string
	Cookie             webauth.CookieOptions
	SessionTTL         time.Duration
	MaxHistoryMessages int

	AttachmentsBucket        string
	AttachmentsPrefix        string
	AttachmentMaxBytes       int64
	AttachmentRetention      int
	AttachmentURLTTL         time.Duration
	AttachmentUserBytes      int64
	AttachmentTenantBytes    int64
	AttachmentPendingPerUser int64

	// WriteRequestsPerMinute bounds per-user console writes. It exists because
	// a user message is persisted before the delegated handler checks quota,
	// so this is the only thing bounding database growth from denied turns.
	WriteRequestsPerMinute int64

	MemoryBudgetTokens int
}

// Dependencies holds everything the web console router needs. A nil optional
// dependency disables just the routes that need it, rather than the console.
type Dependencies struct {
	Config Config
	Logger zerolog.Logger

	OIDC        *webauth.Provider
	Sessions    *webauth.SessionManager
	KeySessions *webauth.KeySessions

	Tenants interface {
		GetByID(context.Context, string) (*pgstore.TenantRecord, error)
	}
	Users interface {
		GetByID(context.Context, string, string) (*pgstore.UserRecord, error)
	}
	Identities    *pgstore.WebIdentityStore
	Conversations *pgstore.WebConversationStore
	Attachments   *pgstore.WebAttachmentStore
	Projects      *pgstore.MemoryProjectStore

	Registry    *providers.Registry
	ModelAccess modelaccess.Store
	ModelGuards modelguard.Rules
	ChatHandler *openai.ChatHandler
	MsgHandler  *anthropic.MessagesHandler

	QuotaManager     *quota.Manager
	QuotaCounter     quotausage.CounterReader
	CacheTrendReader usage.CacheTrendReader

	MemoryBuilder *memory.Builder
	Duffel        *duffel.Client
	Blobs         blob.Store
	Hooks         *hooks.Manager
	WriteLimiter  ratelimit.Store
}

// NewRouter builds the /web/v1 subrouter. It is mounted by the public router
// outside the API-key auth group, since these routes authenticate by cookie.
func NewRouter(deps *Dependencies) http.Handler {
	r := chi.NewRouter()

	if deps == nil || !deps.Config.Enabled || deps.Sessions == nil || (deps.OIDC == nil && deps.KeySessions == nil) {
		r.HandleFunc("/*", unavailableHandler("web console not configured"))
		return r
	}

	// Auth responses (especially Set-Cookie) must not be cached either.
	r.Use(noStore)
	r.Get("/auth/config", authConfigHandler(deps))
	if deps.KeySessions != nil {
		r.Post("/auth/key", keyLoginHandler(deps))
	}
	if deps.OIDC != nil {
		r.Get("/auth/login", loginHandler(deps))
		r.Get("/auth/callback", callbackHandler(deps))
	}

	// Everything else requires a live browser session.
	r.Group(func(r chi.Router) {
		r.Use(middleware.WebSession(deps.Sessions, deps.Config.Cookie))
		r.Post("/auth/logout", logoutHandler(deps))
		r.Get("/me", meHandler(deps))
		// Mirrors the CLI catalog scope: listing models is allowed with either
		// the read-only model scope or any ability to invoke.
		r.With(requireScope(auth.ScopeLLMModelsRead, auth.ScopeLLMInvoke)).
			Get("/models", modelsHandler(deps))
		r.Get("/quota", quotaHandler(deps))
		r.Get("/projects", listProjectsHandler(deps))

		r.Route("/conversations", func(r chi.Router) {
			r.Get("/", listConversationsHandler(deps))
			r.Post("/", createConversationHandler(deps))
			r.Route("/{conversationID}", func(r chi.Router) {
				r.Get("/", getConversationHandler(deps))
				r.Patch("/", updateConversationHandler(deps))
				r.Delete("/", archiveConversationHandler(deps))
				r.Post("/messages", sendMessageHandler(deps))
				r.With(requireScope(auth.ScopeMemoryObserve)).
					Post("/messages/{messageID}/remember", rememberMessageHandler(deps))
			})
		})

		r.Route("/attachments", func(r chi.Router) {
			r.With(requireScope(allTurnScopes()...)).Post("/", createAttachmentHandler(deps))
			r.Post("/{attachmentID}/finalize", finalizeAttachmentHandler(deps))
			r.Get("/{attachmentID}", downloadAttachmentHandler(deps))
		})

		r.Route("/projects/{projectID}", func(r chi.Router) {
			r.With(requireScope(auth.ScopeMemoryContextRead)).Get("/memory", getProjectMemoryHandler(deps))
			r.With(requireScope(auth.ScopeMemoryObserve)).Put("/memory", putProjectMemoryHandler(deps))
			r.With(requireScope(auth.ScopeMemoryObserve)).Post("/lessons", appendProjectLessonHandler(deps))
			r.With(requireScope(auth.ScopeMemoryContextRead)).Get("/bundle", projectBundleHandler(deps))
		})
		r.With(requireScope(auth.ScopeMemoryContextRead)).Get("/memory/search", searchMemoryHandler(deps))
	})

	return r
}

// modelsHandler reuses the tenant-aware catalog the CLI sees, so the console can
// never offer a model the tenant is not entitled to, and then narrows it to the
// protocols a console turn can actually invoke.
//
// The CLI's own catalog is untouched: Pi speaks the passthrough protocols, so
// filtering there would remove models that work. Here the alternative is a
// picker that offers a model and then refuses it at send time.
func modelsHandler(deps *Dependencies) http.HandlerFunc {
	return proxymodels.NewGuardedTenantAwareHandler(deps.Registry, deps.ModelAccess, deps.ModelGuards,
		proxymodels.KeepAPITypes(consoleAPITypes()...))
}

// quotaHandler reuses the self-service quota endpoint verbatim: it reads the
// caller's own auth context and accepts no tenant or user parameters.
func quotaHandler(deps *Dependencies) http.HandlerFunc {
	return quotausage.NewHandler(deps.QuotaManager, deps.QuotaCounter, deps.CacheTrendReader)
}

func unavailableHandler(message string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusServiceUnavailable, "unavailable", message)
	}
}

// noStore keeps authenticated responses out of browser caches. Without an
// explicit directive a response carrying the CSRF token or the caller's
// email may be retained by heuristic caching on a shared computer.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]map[string]string{
		"error": {"type": errType, "message": message},
	})
}
