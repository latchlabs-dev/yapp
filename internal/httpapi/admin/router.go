// Package admin provides the admin API router and handlers.
package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/audit"
	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/httpapi/extensionversion"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/httpapi/proxymodels"
	"github.com/earlgrey/piproxy/internal/id"
	"github.com/earlgrey/piproxy/internal/insights"
	"github.com/earlgrey/piproxy/internal/memory"
	"github.com/earlgrey/piproxy/internal/modelaccess"
	"github.com/earlgrey/piproxy/internal/providerbalance"
	"github.com/earlgrey/piproxy/internal/providers"
	"github.com/earlgrey/piproxy/internal/quota"
	"github.com/earlgrey/piproxy/internal/secrets"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
	redisstore "github.com/earlgrey/piproxy/internal/store/redis"
)

// Dependencies holds admin API dependencies.
type Dependencies struct {
	Logger                   zerolog.Logger
	TenantStore              *pgstore.TenantStore
	UserStore                *pgstore.UserStore
	KeyStore                 *pgstore.KeyStore
	QuotaPolicyStore         *pgstore.QuotaPolicyStore
	ProviderStore            *pgstore.ProviderStore
	ProviderBalanceStore     *pgstore.ProviderBalanceStore
	SubscriptionRateStore    *pgstore.SubscriptionRateStore
	ProviderCredentialStore  *pgstore.ProviderCredentialStore
	ModelAliasStore          *pgstore.ModelAliasStore
	TenantModelSettingsStore *pgstore.TenantModelSettingsStore
	UsageStore               *pgstore.UsageStore
	ConversationPayloadStore *pgstore.ConversationPayloadStore
	CaptureGapStore          *pgstore.CaptureGapStore // nil = capture-gap audit API disabled
	SessionSubmissionStore   *pgstore.SessionSubmissionStore
	ExtensionErrorStore      *pgstore.ExtensionErrorStore
	InsightStore             *pgstore.InsightStore
	InsightClients           *insights.ClientPool
	MemoryProjectStore       ProjectStore
	ProjectUserStore         ProjectUserStore
	AuditLogger              *audit.Logger
	AdminKey                 string // shared secret for admin API access
	RevocationCache          *redisstore.RevocationCache
	RedisClient              *redisstore.Client // read-only access to live quota counters; nil-safe
	CredentialEncryptor      *secrets.Encryptor
	MemoryRuleStore          *pgstore.MemoryRuleStore       // nil = memory rules API disabled
	HookSubscriptionStore    *pgstore.HookSubscriptionStore // nil = hook subscriptions API disabled
	WebIdentityStore         *pgstore.WebIdentityStore      // nil = web console access API disabled
	WebSessionStore          *pgstore.WebSessionStore       // nil = web session revocation disabled
	AllowPrivateHookEgress   bool                           // dev/live-test only; never in production
	DuffelClient             *duffel.Client                 // nil = memory knowledge browser disabled
	MemoryBuilder            *memory.Builder                // nil = injected-bundle preview disabled
	ProviderBalanceRefresher ProviderBalanceRefresher       // nil = upstream balance refresh disabled
	PublicBaseURL            string                         // public-facing URL for generating onboarding instructions
	SessionArchive           SessionArchive                 // nil = session archive API disabled
}

// ProviderBalanceRefresher is implemented by the periodic balance worker and
// kept as an interface so admin router tests do not require live upstreams.
type ProviderBalanceRefresher interface {
	RefreshAccount(ctx context.Context, accountID string) (*providerbalance.Snapshot, error)
}

// NewRouter creates the admin API chi router.
func NewRouter(deps *Dependencies) http.Handler {
	r := chi.NewRouter()

	// Admin auth middleware.
	r.Use(adminAuthMiddleware(deps.AdminKey))
	// Admin payloads (transcripts, reports) are large, repetitive JSON.
	r.Use(middleware.Gzip)

	// Health.
	r.Get("/healthz", healthHandler(deps))

	// Tenants.
	r.Route("/tenants", func(r chi.Router) {
		r.Get("/", listTenantsHandler(deps))
		r.Post("/", createTenantHandler(deps))
		r.Route("/{tenantID}", func(r chi.Router) {
			r.Get("/", getTenantHandler(deps))
			r.Put("/", updateTenantHandler(deps))
			r.Delete("/", deleteTenantHandler(deps))

			// Live spend (current month/day) from the enforcement counters.
			r.Get("/spend", tenantSpendHandler(deps))
			r.Get("/model-settings", getTenantModelSettingsHandler(deps))
			r.Put("/model-settings", updateTenantModelSettingsHandler(deps))

			// Users under tenant.
			r.Route("/users", func(r chi.Router) {
				r.Get("/", listUsersHandler(deps))
				r.Post("/", createUserHandler(deps))
				r.Route("/{userID}", func(r chi.Router) {
					r.Get("/", getUserHandler(deps))
					r.Put("/", updateUserHandler(deps))
					r.Delete("/", deleteUserHandler(deps))

					// Keys under user.
					r.Route("/keys", func(r chi.Router) {
						r.Get("/", listKeysHandler(deps))
						r.Post("/", createKeyHandler(deps))
						r.Route("/{keyID}", func(r chi.Router) {
							r.Put("/revoke", revokeKeyHandler(deps))
						})
					})
				})
			})

			// Onboarding for a user's key (POST with key in body).
			r.Post("/onboarding", onboardingHandler(deps))

			// Quota policies under tenant.
			r.Route("/quota-policies", func(r chi.Router) {
				r.Get("/", listQuotaPoliciesHandler(deps))
				r.Post("/", createQuotaPolicyHandler(deps))
				r.Route("/{policyID}", func(r chi.Router) {
					r.Get("/", getQuotaPolicyHandler(deps))
					r.Put("/", updateQuotaPolicyHandler(deps))
					r.Delete("/", deleteQuotaPolicyHandler(deps))
				})
			})

			// Synthetic models may optionally be tenant-scoped.
			r.Route("/model-aliases", func(r chi.Router) {
				r.Get("/", listModelAliasesHandler(deps))
				r.Post("/", createModelAliasHandler(deps))
				r.Delete("/{aliasID}", deleteModelAliasHandler(deps))
			})
		})
	})

	// Global synthetic models (no tenant).
	r.Route("/model-aliases", func(r chi.Router) {
		r.Get("/", listGlobalModelAliasesHandler(deps))
		r.Post("/", createGlobalModelAliasHandler(deps))
		r.Delete("/{aliasID}", deleteModelAliasHandler(deps))
	})

	r.Get("/model-catalog", modelCatalogHandler(deps))
	r.Get("/provider-catalogs", listProviderCatalogsHandler(deps))

	// Provider accounts (global + tenant-scoped).
	r.Route("/provider-accounts", func(r chi.Router) {
		r.Get("/", listProviderAccountsHandler(deps))
		r.Post("/", createProviderAccountHandler(deps))
		r.Route("/{accountID}", func(r chi.Router) {
			r.Get("/", getProviderAccountHandler(deps))
			r.Put("/", updateProviderAccountHandler(deps))
			r.Delete("/", deleteProviderAccountHandler(deps))
			r.Get("/balance/history", providerBalanceHistoryHandler(deps))
			r.Get("/balance/history/{snapshotID}", providerBalanceSnapshotHandler(deps))
			r.Post("/balance/refresh", refreshProviderBalanceHandler(deps))
			r.Get("/subscription-rate-card", providerSubscriptionRateCardHandler(deps))
			r.Get("/billing-periods", providerBillingPeriodsHandler(deps))
			r.Post("/billing-periods", createProviderBillingPeriodHandler(deps))

			// Credentials under provider account.
			r.Route("/credentials", func(r chi.Router) {
				r.Get("/", listCredentialsHandler(deps))
				r.Post("/", createCredentialHandler(deps))
				r.Route("/{credentialID}", func(r chi.Router) {
					r.Put("/revoke", revokeCredentialHandler(deps))
				})
			})
		})
	})

	// Usage reports.
	r.Get("/usage-reports", usageReportsHandler(deps))

	// Conversation payloads.
	r.Route("/conversation-payloads", func(r chi.Router) {
		r.Get("/", listConversationPayloadsHandler(deps))
		r.Get("/{requestID}", getConversationPayloadHandler(deps))
	})

	// Capture gap markers: durable records of dropped/truncated captures.
	r.Get("/capture-gaps", listCaptureGapsHandler(deps))

	// Session submissions from Pi extensions.
	r.Route("/session-submissions", func(r chi.Router) {
		r.Get("/", listSessionSubmissionsHandler(deps))
	})

	// Session archive: header and turn list from Postgres, transcript pages
	// from object storage. See SESSION_ARCHIVE_PLAN.md.
	r.Route("/sessions/{conversationID}", func(r chi.Router) {
		r.Get("/", getSessionArchiveHandler(deps))
		r.Get("/transcript", getSessionTranscriptHandler(deps))
		r.Post("/rebuild", rebuildSessionTranscriptHandler(deps))
	})

	// Aggregated extension failures (client-side reporting; no raw prompts).
	r.Get("/extension-errors", listExtensionErrorsHandler(deps))

	// Independently deployed insight processors, their matching configurations,
	// the shared tag registry, and generation run history.
	r.Route("/insight-processors", func(r chi.Router) {
		r.Get("/", listInsightProcessorsHandler(deps))
		r.Post("/", createInsightProcessorHandler(deps))
		r.Route("/{processorID}", func(r chi.Router) {
			r.Put("/", updateInsightProcessorHandler(deps))
			r.Delete("/", disableInsightProcessorHandler(deps))
			r.Post("/test", testInsightProcessorHandler(deps))
		})
	})
	r.Route("/insight-configurations", func(r chi.Router) {
		r.Get("/", listInsightConfigurationsHandler(deps))
		r.Post("/", createInsightConfigurationHandler(deps))
		r.Route("/{configurationID}", func(r chi.Router) {
			r.Put("/", updateInsightConfigurationHandler(deps))
			r.Delete("/", disableInsightConfigurationHandler(deps))
		})
	})
	r.Route("/insight-tags", func(r chi.Router) {
		r.Get("/", listInsightTagsHandler(deps))
		r.Post("/", upsertInsightTagHandler(deps))
		r.Delete("/{tagName}", disableInsightTagHandler(deps))
	})
	r.Get("/insight-runs", listInsightRunsHandler(deps))

	// Repository-backed projects. A project is created and deduplicated by its
	// normalized Git remote; memory and distillation data are joined by the
	// resulting stable project ID.
	r.Route("/projects", func(r chi.Router) {
		r.Get("/", listProjectsHandler(deps))
		r.Get("/{projectID}", getProjectHandler(deps))
	})

	// Context injection rules (dynamic backend control of memory injection).
	r.Route("/memory-rules", func(r chi.Router) {
		r.Get("/", listMemoryRulesHandler(deps))
		r.Post("/", upsertMemoryRuleHandler(deps))
		r.Delete("/{ruleID}", deleteMemoryRuleHandler(deps))
	})

	// Semantic search across project memory (embedded duffel index).
	// Web console access control. Nobody can sign into the console until a
	// domain is mapped to a tenant here.
	r.Route("/web-access", func(r chi.Router) {
		r.Get("/domains", listWebDomainsHandler(deps))
		r.Post("/domains", upsertWebDomainHandler(deps))
		r.Delete("/domains/{domain}", deleteWebDomainHandler(deps))
		r.Get("/identities", listWebIdentitiesHandler(deps))
		r.Post("/users/{userID}/revoke-sessions", revokeWebSessionsHandler(deps))
	})

	// Hook subscriptions: DB-managed pre/post hooks, admin-only because
	// include_content and allow_mutation are content-egress and rewrite grants.
	r.Get("/hook-event-types", listHookEventTypesHandler(deps))
	r.Route("/hook-subscriptions", func(r chi.Router) {
		r.Get("/", listHookSubscriptionsHandler(deps))
		r.Post("/", upsertHookSubscriptionHandler(deps))
		r.Route("/{subscriptionID}", func(r chi.Router) {
			r.Put("/", upsertHookSubscriptionHandler(deps))
			r.Delete("/", deleteHookSubscriptionHandler(deps))
			r.Post("/test", testHookSubscriptionHandler(deps))
		})
	})
	r.Get("/hook-deliveries", listHookDeliveriesHandler(deps))

	r.Get("/memory/search", searchMemoryHandler(deps))

	// Project memory knowledge browser (duffel-backed).
	r.Route("/memory/projects", func(r chi.Router) {
		r.Get("/", listMemoryProjectsHandler(deps))
		r.Route("/{project}", func(r chi.Router) {
			r.Get("/", getMemoryProjectHandler(deps))
			r.Post("/archive", archiveMemoryProjectHandler(deps))
			r.Get("/bundle", bundlePreviewHandler(deps))
			r.Put("/memory", putMemoryDocHandler(deps))
			r.Post("/lessons", appendLessonHandler(deps))
			r.Post("/lessons/archive", archiveLessonsHandler(deps))
		})
	})

	return r
}

// --- Admin Auth ---

func adminAuthMiddleware(adminKey string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip auth for health check.
			if r.URL.Path == "/healthz" {
				next.ServeHTTP(w, r)
				return
			}

			apiKey := r.Header.Get("X-Admin-Key")
			bearer := extractBearer(r.Header.Get("Authorization"))

			valid := false
			if adminKey != "" && (apiKey == adminKey || bearer == adminKey) {
				valid = true
			}

			if !valid {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"type":"unauthorized","message":"invalid admin key"}}`))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func extractBearer(authHeader string) string {
	if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
		return authHeader[7:]
	}
	return ""
}

// --- Health ---

func healthHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "admin"})
	}
}

// --- Tenant Handlers ---

func listTenantsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenants, err := deps.TenantStore.List(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_tenants", err.Error())
			return
		}
		if tenants == nil {
			tenants = []*pgstore.TenantRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"tenants": tenants})
	}
}

func createTenantHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.Name == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "name is required")
			return
		}

		now := time.Now()
		record := &pgstore.TenantRecord{
			ID:        id.New("tn_"),
			Name:      req.Name,
			Status:    "active",
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := deps.TenantStore.Insert(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "create_tenant", err.Error())
			return
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionTenantCreated, "tenant", record.ID, "success", "")
		writeJSON(w, http.StatusCreated, record)
	}
}

func getTenantHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		record, err := deps.TenantStore.GetByID(r.Context(), tenantID)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "tenant not found")
			return
		}
		writeJSON(w, http.StatusOK, record)
	}
}

func updateTenantHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		record, err := deps.TenantStore.GetByID(r.Context(), tenantID)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "tenant not found")
			return
		}

		var req struct {
			Name                       *string `json:"name"`
			Status                     *string `json:"status"`
			SessionDistillationEnabled *bool   `json:"session_distillation_enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.Name != nil {
			record.Name = *req.Name
		}
		if req.Status != nil {
			record.Status = *req.Status
		}
		if req.SessionDistillationEnabled != nil {
			record.SessionDistillationEnabled = *req.SessionDistillationEnabled
		}
		if err := deps.TenantStore.Update(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "update_tenant", err.Error())
			return
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionTenantUpdated, "tenant", record.ID, "success", "")
		writeJSON(w, http.StatusOK, record)
	}
}

func deleteTenantHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		if err := deps.TenantStore.Delete(r.Context(), tenantID); err != nil {
			writeError(w, http.StatusInternalServerError, "delete_tenant", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

// --- User Handlers ---

func listUsersHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		users, err := deps.UserStore.List(r.Context(), tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_users", err.Error())
			return
		}
		if users == nil {
			users = []*pgstore.UserRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"users": users})
	}
}

func createUserHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")

		var req struct {
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.Email == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "email is required")
			return
		}

		now := time.Now()
		record := &pgstore.UserRecord{
			ID:          id.New("usr_"),
			TenantID:    tenantID,
			Email:       req.Email,
			DisplayName: req.DisplayName,
			Status:      "active",
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := deps.UserStore.Insert(r.Context(), record); err != nil {
			if errors.Is(err, pgstore.ErrDuplicateUserEmail) {
				writeError(w, http.StatusConflict, "duplicate_email", err.Error())
				return
			}
			writeError(w, http.StatusInternalServerError, "create_user", err.Error())
			return
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionUserCreated, "user", record.ID, "success", "")
		writeJSON(w, http.StatusCreated, record)
	}
}

func getUserHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		userID := chi.URLParam(r, "userID")
		record, err := deps.UserStore.GetByID(r.Context(), tenantID, userID)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "user not found")
			return
		}
		writeJSON(w, http.StatusOK, record)
	}
}

func updateUserHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		userID := chi.URLParam(r, "userID")
		record, err := deps.UserStore.GetByID(r.Context(), tenantID, userID)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "user not found")
			return
		}

		var req struct {
			Email       *string `json:"email"`
			DisplayName *string `json:"display_name"`
			Status      *string `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.Email != nil {
			record.Email = *req.Email
		}
		if req.DisplayName != nil {
			record.DisplayName = *req.DisplayName
		}
		if req.Status != nil {
			record.Status = *req.Status
		}
		if err := deps.UserStore.Update(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "update_user", err.Error())
			return
		}

		writeJSON(w, http.StatusOK, record)
	}
}

func deleteUserHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		userID := chi.URLParam(r, "userID")
		if err := deps.UserStore.Delete(r.Context(), tenantID, userID); err != nil {
			writeError(w, http.StatusInternalServerError, "delete_user", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

// --- Key Handlers ---

func listKeysHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		userID := chi.URLParam(r, "userID")

		records, err := deps.KeyStore.List(r.Context(), tenantID, userID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_keys", err.Error())
			return
		}
		if records == nil {
			records = []*pgstore.KeyRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"keys": records})
	}
}

func createKeyHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		userID := chi.URLParam(r, "userID")

		var req struct {
			Scopes []string `json:"scopes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		req.Scopes = auth.BaselineKeyScopes(req.Scopes)

		// Validate that the user exists.
		if _, err := deps.UserStore.GetByID(r.Context(), tenantID, userID); err != nil {
			writeError(w, http.StatusNotFound, "not_found", fmt.Sprintf("user %s not found in tenant %s", userID, tenantID))
			return
		}

		// Generate key using auth package.
		rawKey, prefix, keyHash, err := auth.GenerateKey("cpk_live_")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "generate_key", err.Error())
			return
		}

		record := &pgstore.KeyRecord{
			ID:        id.New("key_"),
			TenantID:  tenantID,
			UserID:    userID,
			Prefix:    prefix,
			KeyHash:   keyHash,
			Scopes:    req.Scopes,
			Status:    "active",
			CreatedAt: time.Now(),
		}
		if err := deps.KeyStore.Insert(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "create_key", err.Error())
			return
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionKeyCreated, "api_key", record.ID, "success", "")

		// Return the raw key ONCE, plus onboarding instructions.
		resp := map[string]interface{}{
			"key":       rawKey,
			"prefix":    prefix,
			"key_id":    record.ID,
			"tenant_id": tenantID,
			"user_id":   userID,
			"scopes":    req.Scopes,
		}
		if deps.PublicBaseURL != "" {
			resp["onboarding"] = generateOnboarding(deps.PublicBaseURL, rawKey)
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

func revokeKeyHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		keyID := chi.URLParam(r, "keyID")

		// Look up the key to get its prefix for revocation cache.
		keyRecord, err := deps.KeyStore.GetByID(r.Context(), keyID)
		if err != nil {
			writeError(w, http.StatusNotFound, "key_not_found", err.Error())
			return
		}

		if err := deps.KeyStore.Revoke(r.Context(), keyID); err != nil {
			writeError(w, http.StatusInternalServerError, "revoke_key", err.Error())
			return
		}

		// Cache the revocation so auth middleware can reject without Postgres.
		if deps.RevocationCache != nil {
			if err := deps.RevocationCache.MarkRevoked(r.Context(), keyRecord.Prefix); err != nil {
				deps.Logger.Warn().Err(err).Str("prefix", keyRecord.Prefix).Msg("failed to cache key revocation")
			}
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionKeyRevoked, "api_key", keyID, "success", "")
		writeJSON(w, http.StatusOK, map[string]string{"status": "revoked", "key_id": keyID})
	}
}

// --- Quota Policy Handlers ---

func listQuotaPoliciesHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		policies, err := deps.QuotaPolicyStore.List(r.Context(), tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_quota_policies", err.Error())
			return
		}
		if policies == nil {
			policies = []*pgstore.QuotaPolicyRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"quota_policies": policies})
	}
}

func createQuotaPolicyHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		var req struct {
			ScopeType string          `json:"scope_type"`
			ScopeID   string          `json:"scope_id"`
			Priority  int             `json:"priority"`
			Limits    json.RawMessage `json:"limits"`
			Enabled   bool            `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.ScopeType == "" || req.ScopeID == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "scope_type and scope_id are required")
			return
		}
		if len(req.Limits) == 0 {
			req.Limits = json.RawMessage(`{}`)
		}

		now := time.Now()
		record := &pgstore.QuotaPolicyRecord{
			ID:        id.New("qp_"),
			TenantID:  tenantID,
			ScopeType: req.ScopeType,
			ScopeID:   req.ScopeID,
			Priority:  req.Priority,
			Limits:    req.Limits,
			Enabled:   true,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := deps.QuotaPolicyStore.Insert(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "create_quota_policy", err.Error())
			return
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionQuotaCreated, "quota_policy", record.ID, "success", "")
		writeJSON(w, http.StatusCreated, record)
	}
}

func getQuotaPolicyHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		policyID := chi.URLParam(r, "policyID")
		record, err := deps.QuotaPolicyStore.GetByID(r.Context(), tenantID, policyID)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "quota policy not found")
			return
		}
		writeJSON(w, http.StatusOK, record)
	}
}

func updateQuotaPolicyHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		policyID := chi.URLParam(r, "policyID")
		record, err := deps.QuotaPolicyStore.GetByID(r.Context(), tenantID, policyID)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "quota policy not found")
			return
		}

		var req struct {
			ScopeType *string          `json:"scope_type"`
			ScopeID   *string          `json:"scope_id"`
			Priority  *int             `json:"priority"`
			Limits    *json.RawMessage `json:"limits"`
			Enabled   *bool            `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.ScopeType != nil {
			record.ScopeType = *req.ScopeType
		}
		if req.ScopeID != nil {
			record.ScopeID = *req.ScopeID
		}
		if req.Priority != nil {
			record.Priority = *req.Priority
		}
		if req.Limits != nil {
			record.Limits = *req.Limits
		}
		if req.Enabled != nil {
			record.Enabled = *req.Enabled
		}

		if err := deps.QuotaPolicyStore.Update(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "update_quota_policy", err.Error())
			return
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionQuotaUpdated, "quota_policy", record.ID, "success", "")
		writeJSON(w, http.StatusOK, record)
	}
}

func deleteQuotaPolicyHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		policyID := chi.URLParam(r, "policyID")
		if err := deps.QuotaPolicyStore.Delete(r.Context(), tenantID, policyID); err != nil {
			writeError(w, http.StatusInternalServerError, "delete_quota_policy", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

func getTenantModelSettingsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.TenantModelSettingsStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "tenant model settings are not configured")
			return
		}
		tenantID := chi.URLParam(r, "tenantID")
		if _, err := deps.TenantStore.GetByID(r.Context(), tenantID); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "tenant not found")
			return
		}
		cfg, err := deps.TenantModelSettingsStore.Get(r.Context(), tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "get_tenant_model_settings", err.Error())
			return
		}
		if cfg.ModelIDs == nil {
			cfg.ModelIDs = []string{}
		}
		writeJSON(w, http.StatusOK, cfg)
	}
}

func updateTenantModelSettingsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.TenantModelSettingsStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "tenant model settings are not configured")
			return
		}
		tenantID := chi.URLParam(r, "tenantID")
		if _, err := deps.TenantStore.GetByID(r.Context(), tenantID); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "tenant not found")
			return
		}
		var req struct {
			Mode     string   `json:"mode"`
			ModelIDs []string `json:"model_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		rawMode := strings.ToLower(strings.TrimSpace(req.Mode))
		mode := modelaccess.NormalizeMode(rawMode)
		if rawMode != "" && rawMode != modelaccess.ModeBlacklist && rawMode != modelaccess.ModeWhitelist {
			writeError(w, http.StatusBadRequest, "validation_error", "mode must be blacklist or whitelist")
			return
		}
		cfg, err := deps.TenantModelSettingsStore.Upsert(r.Context(), &modelaccess.Config{
			TenantID: tenantID,
			Mode:     mode,
			ModelIDs: req.ModelIDs,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "update_tenant_model_settings", err.Error())
			return
		}
		deps.AuditLogger.Logf("admin", "admin", audit.ActionTenantUpdated, "tenant_model_settings", tenantID, "success", "")
		writeJSON(w, http.StatusOK, cfg)
	}
}

// --- Model Alias Handlers ---

func listModelAliasesHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		aliases, err := deps.ModelAliasStore.List(r.Context(), tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_model_aliases", err.Error())
			return
		}
		if aliases == nil {
			aliases = []*pgstore.ModelAliasRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"model_aliases": aliases})
	}
}

func createModelAliasHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		createAlias(deps, w, r, tenantID)
	}
}

func deleteModelAliasHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		aliasID := chi.URLParam(r, "aliasID")
		if err := deps.ModelAliasStore.Delete(r.Context(), aliasID); err != nil {
			writeError(w, http.StatusInternalServerError, "delete_model_alias", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

// Global model alias handlers (no tenant scope).

func listGlobalModelAliasesHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var (
			aliases []*pgstore.ModelAliasRecord
			err     error
		)
		if r.URL.Query().Get("scope") == "all" {
			aliases, err = deps.ModelAliasStore.ListAll(r.Context())
		} else {
			aliases, err = deps.ModelAliasStore.List(r.Context(), "")
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_model_aliases", err.Error())
			return
		}
		if aliases == nil {
			aliases = []*pgstore.ModelAliasRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"model_aliases": aliases})
	}
}

func createGlobalModelAliasHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		createAlias(deps, w, r, "")
	}
}

// createAlias is the shared alias creation logic.
func createAlias(deps *Dependencies, w http.ResponseWriter, r *http.Request, tenantID string) {
	var req struct {
		PublicModel       string `json:"public_model"`
		UpstreamModel     string `json:"upstream_model"`
		ProviderAccountID string `json:"provider_account_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	req.PublicModel = strings.TrimSpace(req.PublicModel)
	req.UpstreamModel = strings.TrimSpace(req.UpstreamModel)
	req.ProviderAccountID = strings.TrimSpace(req.ProviderAccountID)
	if req.PublicModel == "" || req.ProviderAccountID == "" || req.UpstreamModel == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "public_model, provider_account_id, and upstream_model are required")
		return
	}
	if !validSyntheticModelName(req.PublicModel) {
		writeError(w, http.StatusBadRequest, "validation_error", "public_model must be 1-128 letters, numbers, dots, underscores, slashes, colons, or hyphens")
		return
	}
	if deps.ProviderStore == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "provider store is not configured")
		return
	}
	runtimeProviders, err := deps.ProviderStore.ListProviders(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_provider_models", err.Error())
		return
	}
	targetProvider, targetModel, ok := resolveSyntheticTarget(runtimeProviders, req.ProviderAccountID, req.UpstreamModel)
	if !ok {
		writeError(w, http.StatusBadRequest, "validation_error", "target provider account or model is not active and discoverable")
		return
	}
	if syntheticNameCollidesWithCatalog(runtimeProviders, req.PublicModel) {
		writeError(w, http.StatusConflict, "model_name_conflict", "synthetic model name conflicts with a catalog model")
		return
	}
	existing, err := deps.ModelAliasStore.ListAll(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list_model_aliases", err.Error())
		return
	}
	for _, alias := range existing {
		if alias.TenantID == tenantID && alias.PublicModel == req.PublicModel {
			writeError(w, http.StatusConflict, "model_name_conflict", "a synthetic model with this name already exists in the selected scope")
			return
		}
	}
	publicProvider := publicProviderKey(targetProvider.PublicBasePath)
	if publicProvider == "" {
		writeError(w, http.StatusBadRequest, "validation_error", "target provider does not have a public routing path")
		return
	}
	upstreamModel := targetModel.UpstreamID
	if upstreamModel == "" {
		upstreamModel = targetModel.PublicID
	}

	now := time.Now()
	record := &pgstore.ModelAliasRecord{
		ID:                id.New("ali_"),
		TenantID:          tenantID,
		PublicProvider:    publicProvider,
		PublicModel:       req.PublicModel,
		UpstreamProvider:  string(targetProvider.Type),
		UpstreamModel:     upstreamModel,
		ProviderAccountID: req.ProviderAccountID,
		APIType:           string(targetProvider.Type),
		Capabilities:      json.RawMessage(`{}`),
		Cost:              json.RawMessage(`{}`),
		Limits:            json.RawMessage(`{}`),
		Status:            "active",
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if err := deps.ModelAliasStore.Insert(r.Context(), record); err != nil {
		writeError(w, http.StatusInternalServerError, "create_model_alias", err.Error())
		return
	}

	deps.AuditLogger.Logf("admin", "admin", audit.ActionAliasCreated, "model_alias", record.ID, "success", "")
	writeJSON(w, http.StatusCreated, record)
}

func resolveSyntheticTarget(runtimeProviders []*providers.Provider, accountID, modelID string) (*providers.Provider, providers.Model, bool) {
	for _, provider := range runtimeProviders {
		if provider == nil || provider.ID != accountID || !provider.Enabled || provider.CredentialID == "" {
			continue
		}
		for _, model := range provider.Models {
			if model.PublicID == modelID || model.UpstreamID == modelID {
				return provider, model, true
			}
		}
	}
	return nil, providers.Model{}, false
}

func syntheticNameCollidesWithCatalog(runtimeProviders []*providers.Provider, name string) bool {
	for _, provider := range runtimeProviders {
		if provider == nil || !provider.Enabled || provider.CredentialID == "" {
			continue
		}
		for _, model := range provider.Models {
			if model.PublicID == name || model.UpstreamID == name {
				return true
			}
		}
	}
	return false
}

func publicProviderKey(publicBasePath string) string {
	path := strings.Trim(publicBasePath, "/")
	if path == "" {
		return ""
	}
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

func validSyntheticModelName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for i, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '.' || r == '_' || r == '/' || r == ':' || r == '-' {
			continue
		}
		if i == 0 {
			return false
		}
		return false
	}
	return true
}

// --- Provider Account Handlers ---

func listProviderAccountsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := r.URL.Query().Get("tenant_id")
		accounts, err := deps.ProviderStore.ListProviderAccounts(r.Context(), tenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_provider_accounts", err.Error())
			return
		}
		if accounts == nil {
			accounts = []*pgstore.ProviderAccountRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"provider_accounts": accounts})
	}
}

func createProviderAccountHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			TenantID string                 `json:"tenant_id,omitempty"`
			Provider string                 `json:"provider"`
			Name     string                 `json:"name"`
			Config   map[string]interface{} `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.Provider == "" || req.Name == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "provider and name are required")
			return
		}
		if req.Config == nil {
			req.Config = map[string]interface{}{}
		}
		applyCodexSubscriptionDefaults(req.Provider, req.Config)
		if err := validateBillingConfig(req.Config); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_billing_config", err.Error())
			return
		}

		now := time.Now()
		record := &pgstore.ProviderAccountRecord{
			ID:        id.New("prv_"),
			TenantID:  req.TenantID,
			Provider:  req.Provider,
			Name:      req.Name,
			Status:    "active",
			Config:    req.Config,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if err := deps.ProviderStore.InsertProviderAccount(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "create_provider_account", err.Error())
			return
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionProviderCreated, "provider_account", record.ID, "success", "")
		writeJSON(w, http.StatusCreated, record)
	}
}

func applyCodexSubscriptionDefaults(provider string, config map[string]interface{}) {
	if provider != "openai-codex" && provider != "openai-codex-responses" {
		return
	}
	billing, _ := config["billing"].(map[string]interface{})
	if billing == nil {
		billing = map[string]interface{}{}
		config["billing"] = billing
	}
	billing["mode"] = "subscription"
	if billing["currency"] == nil || billing["currency"] == "" {
		billing["currency"] = "USD"
	}
	if cost, err := numericConfigValue(billing["monthly_cost_cents"]); err != nil || cost <= 0 {
		billing["monthly_cost_cents"] = float64(20000)
	}
}

func getProviderAccountHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := chi.URLParam(r, "accountID")
		record, err := deps.ProviderStore.GetProviderAccount(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "provider account not found")
			return
		}
		writeJSON(w, http.StatusOK, record)
	}
}

func updateProviderAccountHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := chi.URLParam(r, "accountID")
		record, err := deps.ProviderStore.GetProviderAccount(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusNotFound, "not_found", "provider account not found")
			return
		}

		var req struct {
			Provider *string                 `json:"provider"`
			Name     *string                 `json:"name"`
			Status   *string                 `json:"status"`
			Config   *map[string]interface{} `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.Provider != nil {
			record.Provider = *req.Provider
		}
		if req.Name != nil {
			record.Name = *req.Name
		}
		if req.Status != nil {
			record.Status = *req.Status
		}
		if req.Config != nil {
			if oldBilling, ok := record.Config["billing"].(map[string]interface{}); ok && oldBilling["allocation_method"] == "observed_delta" {
				nextBilling, ok := (*req.Config)["billing"].(map[string]interface{})
				if !ok || nextBilling["allocation_method"] != "observed_delta" || nextBilling["mode"] != "subscription" || nextBilling["currency"] != "USD" {
					writeError(w, http.StatusBadRequest, "invalid_billing_config", "paid-period accounting must be retained for this subscription")
					return
				}
			}
			if err := validateBillingConfig(*req.Config); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_billing_config", err.Error())
				return
			}
			record.Config = *req.Config
		}

		if err := deps.ProviderStore.UpdateProviderAccount(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "update_provider_account", err.Error())
			return
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionProviderUpdated, "provider_account", record.ID, "success", "")
		writeJSON(w, http.StatusOK, record)
	}
}

func validateBillingConfig(config map[string]interface{}) error {
	billingValue, exists := config["billing"]
	if !exists {
		return nil
	}
	billing, ok := billingValue.(map[string]interface{})
	if !ok {
		return fmt.Errorf("billing must be an object")
	}
	mode, _ := billing["mode"].(string)
	if mode != "" && mode != "default" && mode != "metered" && mode != "subscription" {
		return fmt.Errorf("billing mode must be default, metered, or subscription")
	}
	currency, _ := billing["currency"].(string)
	if currency != "" && (len(currency) != 3 || strings.ToUpper(currency) != currency) {
		return fmt.Errorf("billing currency must be a three-letter uppercase code")
	}
	monthlyCost, err := numericConfigValue(billing["monthly_cost_cents"])
	if err != nil || monthlyCost < 0 {
		return fmt.Errorf("monthly subscription cost must be a non-negative number of cents")
	}
	if mode == "subscription" && monthlyCost <= 0 {
		return fmt.Errorf("subscription billing requires a positive monthly cost")
	}
	fallback, err := numericConfigValue(billing["fallback_usd_per_million_tokens"])
	if err != nil || fallback < 0 {
		return fmt.Errorf("fallback rate must be a non-negative USD amount per million tokens")
	}
	return nil
}

func numericConfigValue(value interface{}) (float64, error) {
	if value == nil {
		return 0, nil
	}
	switch number := value.(type) {
	case float64:
		return number, nil
	case float32:
		return float64(number), nil
	case int:
		return float64(number), nil
	case int64:
		return float64(number), nil
	case json.Number:
		return number.Float64()
	default:
		return 0, fmt.Errorf("not numeric")
	}
}

func deleteProviderAccountHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := chi.URLParam(r, "accountID")
		if err := deps.ProviderStore.DeleteProviderAccount(r.Context(), accountID); err != nil {
			writeError(w, http.StatusInternalServerError, "delete_provider_account", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
	}
}

func refreshProviderBalanceHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ProviderBalanceRefresher == nil {
			writeError(w, http.StatusServiceUnavailable, "balance_refresh_unavailable", "provider balance refresh is unavailable")
			return
		}
		accountID := chi.URLParam(r, "accountID")
		snapshot, err := deps.ProviderBalanceRefresher.RefreshAccount(r.Context(), accountID)
		if snapshot == nil {
			if err != nil {
				writeError(w, http.StatusBadGateway, "balance_refresh_failed", err.Error())
				return
			}
			writeError(w, http.StatusBadRequest, "balance_refresh_disabled", "balance refresh is disabled or unsupported for this provider")
			return
		}
		// Return the recorded error snapshot as useful state. The UI can retain
		// the timestamp and diagnostic without treating a transient upstream
		// failure as a transport failure.
		writeJSON(w, http.StatusOK, snapshot)
	}
}

func providerBalanceHistoryHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ProviderBalanceStore == nil {
			writeError(w, http.StatusServiceUnavailable, "balance_history_unavailable", "provider balance history is unavailable")
			return
		}
		query, err := providerbalance.ParseHistoryQuery(r.URL.Query())
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_history_query", err.Error())
			return
		}
		history, err := deps.ProviderBalanceStore.ListBalanceHistory(r.Context(), chi.URLParam(r, "accountID"), query)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_provider_balance_history", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, providerbalance.BuildHistory(history, query))
	}
}

func providerBalanceSnapshotHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ProviderBalanceStore == nil {
			writeError(w, http.StatusServiceUnavailable, "balance_history_unavailable", "provider balance history is unavailable")
			return
		}
		snapshotID, err := strconv.ParseInt(chi.URLParam(r, "snapshotID"), 10, 64)
		if err != nil || snapshotID <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_snapshot_id", "snapshot id must be a positive integer")
			return
		}
		snapshot, err := deps.ProviderBalanceStore.GetBalanceSnapshot(
			r.Context(), chi.URLParam(r, "accountID"), snapshotID,
		)
		if err != nil {
			writeError(w, http.StatusNotFound, "provider_balance_snapshot_not_found", "provider balance snapshot not found")
			return
		}
		writeJSON(w, http.StatusOK, snapshot)
	}
}

func providerSubscriptionRateCardHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SubscriptionRateStore == nil {
			writeError(w, http.StatusServiceUnavailable, "subscription_rate_card_unavailable", "subscription rate cards are unavailable")
			return
		}
		rateDate := time.Now().UTC()
		if rawDate := r.URL.Query().Get("date"); rawDate != "" {
			parsed, err := time.Parse("2006-01-02", rawDate)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_rate_date", "date must use YYYY-MM-DD")
				return
			}
			rateDate = parsed
		}
		cards, err := deps.SubscriptionRateStore.ListRateCards(r.Context(), chi.URLParam(r, "accountID"), rateDate)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_subscription_rate_cards", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"rate_cards": cards})
	}
}

// --- Credential Handlers ---

func listCredentialsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := chi.URLParam(r, "accountID")
		creds, err := deps.ProviderCredentialStore.List(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_credentials", err.Error())
			return
		}
		if creds == nil {
			creds = []*pgstore.ProviderCredentialRecord{}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"credentials": creds})
	}
}

func createCredentialHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID := chi.URLParam(r, "accountID")

		// api_key covers the common case (static provider keys). The OAuth2
		// fields support Codex/ChatGPT credentials, whose access token expires
		// and whose refresh token rotates, so the full token pair is stored.
		var req struct {
			APIKey        string     `json:"api_key"`
			AccessToken   string     `json:"access_token"`
			RefreshToken  string     `json:"refresh_token"`
			AccountID     string     `json:"account_id"`
			OAuthTokenURL string     `json:"oauth_token_url"`
			OAuthClientID string     `json:"oauth_client_id"`
			OAuthScope    string     `json:"oauth_scope"`
			ExpiresAt     *time.Time `json:"expires_at"`
			CreatedBy     string     `json:"created_by"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.APIKey == "" && req.AccessToken == "" && req.RefreshToken == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "api_key, access_token, or refresh_token is required")
			return
		}
		if req.CreatedBy == "" {
			req.CreatedBy = "admin"
		}
		if deps.CredentialEncryptor == nil {
			writeError(w, http.StatusInternalServerError, "credential_encryption_unconfigured", "credential encryption key is not configured")
			return
		}

		var accountConfig map[string]interface{}
		providerType := ""
		if deps.ProviderStore != nil {
			acct, err := deps.ProviderStore.GetProviderAccount(r.Context(), accountID)
			if err == nil {
				providerType = acct.Provider
				accountConfig = acct.Config
			}
		}

		oauthTokenURL := firstNonEmpty(req.OAuthTokenURL, configString(accountConfig, "oauth_token_url"))
		oauthClientID := firstNonEmpty(req.OAuthClientID, configString(accountConfig, "oauth_client_id"))
		oauthScope := firstNonEmpty(req.OAuthScope, configString(accountConfig, "oauth_scope"))
		if strings.TrimSpace(req.RefreshToken) != "" {
			if strings.TrimSpace(oauthTokenURL) == "" || strings.TrimSpace(oauthClientID) == "" {
				writeError(w, http.StatusBadRequest, "validation_error", "oauth_token_url and oauth_client_id are required for refresh_token credentials")
				return
			}
		}

		cred := secrets.Credential{
			APIKey:        req.APIKey,
			AccessToken:   req.AccessToken,
			RefreshToken:  req.RefreshToken,
			AccountID:     req.AccountID,
			ProviderType:  providerType,
			OAuthTokenURL: oauthTokenURL,
			OAuthClientID: oauthClientID,
			OAuthScope:    oauthScope,
		}
		if req.ExpiresAt != nil {
			cred.ExpiresAt = *req.ExpiresAt
		}

		payload, err := json.Marshal(cred)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "credential_payload", err.Error())
			return
		}
		encryptedBlob, err := deps.CredentialEncryptor.Encrypt(payload)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "credential_encrypt", err.Error())
			return
		}
		version, err := deps.ProviderCredentialStore.NextVersion(r.Context(), accountID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "credential_version", err.Error())
			return
		}

		now := time.Now()
		record := &pgstore.ProviderCredentialRecord{
			ID:                id.New("pcr_"),
			ProviderAccountID: accountID,
			Version:           version,
			Status:            "active",
			EncryptedBlob:     encryptedBlob,
			KMSKeyID:          "local-db",
			Fingerprint:       secrets.CredentialFingerprint(cred.SecretValue()),
			ValidFrom:         &now,
			CreatedBy:         req.CreatedBy,
			CreatedAt:         now,
		}

		if err := deps.ProviderCredentialStore.InsertActive(r.Context(), record); err != nil {
			writeError(w, http.StatusInternalServerError, "create_credential", err.Error())
			return
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionCredentialCreated, "provider_credential", record.ID, "success", "")
		writeJSON(w, http.StatusCreated, record)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func configString(config map[string]interface{}, key string) string {
	if config == nil {
		return ""
	}
	value, _ := config[key].(string)
	return value
}

func revokeCredentialHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		credentialID := chi.URLParam(r, "credentialID")
		if err := deps.ProviderCredentialStore.Revoke(r.Context(), credentialID); err != nil {
			writeError(w, http.StatusInternalServerError, "revoke_credential", err.Error())
			return
		}

		deps.AuditLogger.Logf("admin", "admin", audit.ActionCredentialRevoked, "provider_credential", credentialID, "success", "")
		writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	}
}

// --- Usage Reports ---

func usageReportsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		tenantID := q.Get("tenant_id")
		userID := q.Get("user_id")
		model := q.Get("model")
		granularity := q.Get("granularity")
		if granularity == "" {
			granularity = "daily"
		}

		// Parse time range with defaults (last 7 days).
		now := time.Now().UTC()
		from := parseTimeParam(q.Get("from"), now.AddDate(0, 0, -7))
		to := parseTimeParam(q.Get("to"), now)
		if err := validateUsageReportRange(from, to, granularity); err != nil {
			writeError(w, http.StatusBadRequest, "range_too_large", err.Error())
			return
		}

		// limit caps the returned rows; 0 (or unparseable) lets the store apply
		// its own default/clamp.
		limit, _ := strconv.Atoi(q.Get("limit"))

		reports, err := deps.UsageStore.QueryUsageReports(r.Context(), tenantID, from, to, granularity, userID, model, limit)
		if err != nil {
			// writeError only writes the HTTP response; the admin router has no
			// request-logging middleware, so log server faults here or they are
			// invisible in the server logs (only visible in the client's network tab).
			deps.Logger.Error().Err(err).
				Str("granularity", granularity).Str("tenant_id", tenantID).
				Msg("usage reports query failed")
			writeError(w, http.StatusInternalServerError, "query_usage_reports", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"usage_reports": reports,
			"from":          from.Format(time.RFC3339),
			"to":            to.Format(time.RFC3339),
			"granularity":   granularity,
		})
	}
}

// --- Conversation Payloads ---

// conversationPayloadResponse is the full payload including decompressed text.
type conversationPayloadResponse struct {
	ID                string     `json:"id"`
	RequestID         string     `json:"request_id"`
	ConversationID    string     `json:"conversation_id,omitempty"`
	TenantID          string     `json:"tenant_id,omitempty"`
	UserID            string     `json:"user_id,omitempty"`
	KeyID             string     `json:"key_id,omitempty"`
	Provider          string     `json:"provider"`
	Model             string     `json:"model"`
	Status            string     `json:"status"`
	ErrorClass        string     `json:"error_class,omitempty"`
	Streaming         bool       `json:"streaming"`
	RequestBody       string     `json:"request_body"`
	ResponseBody      string     `json:"response_body"`
	ReasoningContent  string     `json:"reasoning_content,omitempty"`
	RequestTruncated  bool       `json:"request_truncated"`
	ResponseTruncated bool       `json:"response_truncated"`
	ResponseFormat    string     `json:"response_format"`
	CreatedAt         time.Time  `json:"created_at"`
	CompletedAt       *time.Time `json:"completed_at,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
}

func toPayloadResponse(p *capture.Payload) *conversationPayloadResponse {
	return &conversationPayloadResponse{
		ID:                p.ID,
		RequestID:         p.RequestID,
		ConversationID:    p.ConversationID,
		TenantID:          p.TenantID,
		UserID:            p.UserID,
		KeyID:             p.KeyID,
		Provider:          p.Provider,
		Model:             p.Model,
		Status:            p.Status,
		ErrorClass:        p.ErrorClass,
		Streaming:         p.Streaming,
		RequestBody:       string(p.RequestBody),
		ResponseBody:      string(p.ResponseBody),
		ReasoningContent:  extractReasoningContent(string(p.ResponseBody), p.ResponseFormat),
		RequestTruncated:  p.RequestTruncated,
		ResponseTruncated: p.ResponseTruncated,
		ResponseFormat:    p.ResponseFormat,
		CreatedAt:         p.CreatedAt,
		CompletedAt:       p.CompletedAt,
		ExpiresAt:         p.ExpiresAt,
	}
}

// extractReasoningContent pulls the chain-of-thought reasoning text out of a
// captured upstream response so it can be surfaced as its own field rather than
// staying buried in the raw response body. It handles both the streaming SSE
// form (concatenated choices[].delta.reasoning_content deltas) and the
// non-streaming JSON form (choices[].message.reasoning_content). Returns "" when
// no reasoning is present (the field is omitempty, so it then drops out of JSON).
func extractReasoningContent(body, responseFormat string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return ""
	}

	// Streaming responses: accumulate reasoning_content deltas across SSE events.
	if responseFormat == "sse" || strings.HasPrefix(body, "data:") || strings.Contains(body, "\ndata:") {
		var b strings.Builder
		for _, line := range strings.Split(body, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var ev struct {
				Choices []struct {
					Delta struct {
						ReasoningContent string `json:"reasoning_content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if err := json.Unmarshal([]byte(data), &ev); err != nil {
				continue
			}
			for _, c := range ev.Choices {
				b.WriteString(c.Delta.ReasoningContent)
			}
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			return s
		}
	}

	// Non-streaming JSON response: choices[].message.reasoning_content.
	var resp struct {
		Choices []struct {
			Message struct {
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err == nil {
		var b strings.Builder
		for _, c := range resp.Choices {
			b.WriteString(c.Message.ReasoningContent)
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			return s
		}
	}

	return ""
}

func listConversationPayloadsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		requestID := q.Get("request_id")
		conversationID := q.Get("conversation_id")
		tenantID := q.Get("tenant_id")
		userID := q.Get("user_id")
		provider := q.Get("provider")
		includePrompts := q.Get("include_prompts") == "true"
		groupBy := q.Get("group_by")
		limit := 50
		if l := q.Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
				limit = n
			}
		}
		offset := 0
		if o := q.Get("offset"); o != "" {
			if n, err := strconv.Atoi(o); err == nil && n >= 0 {
				offset = n
			}
		}

		// Session mode: group by conversation_id, paginate at session level.
		if groupBy == "session" {
			total, sessions, err := deps.ConversationPayloadStore.ListSessions(r.Context(), tenantID, userID, provider, limit, offset)
			if err != nil {
				deps.Logger.Error().Err(err).
					Str("tenant_id", tenantID).Str("user_id", userID).
					Msg("conversation sessions list failed")
				writeError(w, http.StatusInternalServerError, "list_conversation_sessions", err.Error())
				return
			}
			writeJSON(w, http.StatusOK, map[string]interface{}{
				"sessions":    sessions,
				"total_count": total,
			})
			return
		}

		var scopes []pgstore.PayloadSessionScope
		if q.Has("session_scope") {
			var scope pgstore.PayloadSessionScope
			if err := json.Unmarshal([]byte(q.Get("session_scope")), &scope); err != nil || scope.From.IsZero() || scope.To.IsZero() || scope.To.Before(scope.From) {
				writeError(w, http.StatusBadRequest, "invalid_session_scope", "session_scope requires a valid from/to interval")
				return
			}
			scopes = append(scopes, scope)
		}
		total, summaries, err := deps.ConversationPayloadStore.List(r.Context(), requestID, conversationID, tenantID, userID, provider, limit, offset, includePrompts, scopes...)
		if err != nil {
			deps.Logger.Error().Err(err).
				Str("request_id", requestID).Str("tenant_id", tenantID).Str("user_id", userID).
				Msg("conversation payloads list failed")
			writeError(w, http.StatusInternalServerError, "list_conversation_payloads", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"conversation_payloads": summaries,
			"total_count":           total,
		})
	}
}

func listCaptureGapsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.CaptureGapStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "capture gap store is not configured")
			return
		}
		q := r.URL.Query()
		limit := 50
		if l := q.Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 200 {
				limit = n
			}
		}
		offset := 0
		if o := q.Get("offset"); o != "" {
			if n, err := strconv.Atoi(o); err == nil && n >= 0 {
				offset = n
			}
		}
		gaps, total, err := deps.CaptureGapStore.List(
			r.Context(), q.Get("conversation_id"), q.Get("tenant_id"), q.Get("reason"), limit, offset)
		if err != nil {
			deps.Logger.Error().Err(err).
				Str("conversation_id", q.Get("conversation_id")).
				Str("tenant_id", q.Get("tenant_id")).
				Msg("capture gaps list failed")
			writeError(w, http.StatusInternalServerError, "list_capture_gaps", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"capture_gaps": gaps,
			"total_count":  total,
		})
	}
}

func getConversationPayloadHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := chi.URLParam(r, "requestID")
		if requestID == "" {
			writeError(w, http.StatusBadRequest, "missing_request_id", "request_id is required")
			return
		}

		payload, err := deps.ConversationPayloadStore.Get(r.Context(), requestID)
		if err != nil {
			deps.Logger.Error().Err(err).Str("request_id", requestID).
				Msg("conversation payload get failed")
			writeError(w, http.StatusNotFound, "conversation_payload_not_found", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, toPayloadResponse(payload))
	}
}

// scopeSpend is the live spend for one quota scope, in cents.
type scopeSpend struct {
	ScopeType         string `json:"scope_type"`
	ScopeID           string `json:"scope_id"`
	MonthlySpendCents int64  `json:"monthly_spend_cents"`
	DailySpendCents   int64  `json:"daily_spend_cents"`
	MonthlyLimitCents int64  `json:"monthly_limit_cents"`
	DailyLimitCents   int64  `json:"daily_limit_cents"`
}

// tenantSpendHandler returns the live monthly/daily spend for the tenant and
// each of its users, read directly from the Redis enforcement counters. This is
// the exact state the quota enforcer checks against (reservations reconciled to
// actual spend on request completion), so it can differ slightly from recorded
// usage in usage_events. Counters are stored in millicents; we return cents.
func tenantSpendHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenantID := chi.URLParam(r, "tenantID")
		now := time.Now().UTC()
		monthStr := now.Format("2006-01")
		dateStr := now.Format("2006-01-02")

		redisOK := deps.RedisClient != nil && deps.RedisClient.IsConnected()
		users, usersErr := deps.UserStore.List(r.Context(), tenantID)
		if usersErr != nil {
			deps.Logger.Error().Err(usersErr).Str("tenant_id", tenantID).
				Msg("tenant spend failed to list users")
			writeError(w, http.StatusInternalServerError, "list_users", usersErr.Error())
			return
		}

		// Rebuild the same effective policy snapshot as the data plane, including
		// explicit user spend policies. Returning limits beside the
		// counters prevents the UI from treating disabled policies (or the whole
		// tenant budget) as a user's enforced allowance.
		manager := quota.NewManager()
		if deps.QuotaPolicyStore != nil {
			records, err := deps.QuotaPolicyStore.List(r.Context(), tenantID)
			if err != nil {
				deps.Logger.Error().Err(err).Str("tenant_id", tenantID).
					Msg("tenant spend failed to list quota policies")
				writeError(w, http.StatusInternalServerError, "list_quota_policies", err.Error())
				return
			}
			policies := make([]*quota.Policy, 0, len(records))
			for _, record := range records {
				policy, err := record.ToPolicy()
				if err != nil {
					deps.Logger.Warn().Err(err).Str("policy_id", record.ID).
						Msg("tenant spend skipping invalid quota policy")
					continue
				}
				policies = append(policies, policy)
			}
			manager.SetPolicies(policies)
		}

		read := func(scopeType, scopeID string) scopeSpend {
			scope := quota.ScopeType(scopeType)
			s := scopeSpend{
				ScopeType:         scopeType,
				ScopeID:           scopeID,
				MonthlyLimitCents: manager.GetEffectiveLimit(scope, scopeID, quota.LimitMonthlySpendCents),
				DailyLimitCents:   manager.GetEffectiveLimit(scope, scopeID, quota.LimitDailySpendCents),
			}
			if !redisOK {
				return s
			}
			monthKey := fmt.Sprintf("quota:monthly:spend_millicents:%s:%s:%s", scopeType, scopeID, monthStr)
			dayKey := fmt.Sprintf("quota:daily:spend_millicents:%s:%s:%s", scopeType, scopeID, dateStr)
			if mc, err := deps.RedisClient.GetCounter(r.Context(), monthKey); err == nil {
				s.MonthlySpendCents = mc / quota.SpendCounterScale
			}
			if dc, err := deps.RedisClient.GetCounter(r.Context(), dayKey); err == nil {
				s.DailySpendCents = dc / quota.SpendCounterScale
			}
			return s
		}

		scopes := []scopeSpend{read(string(quota.ScopeTenant), tenantID)}
		for _, u := range users {
			scopes = append(scopes, read(string(quota.ScopeUser), u.ID))
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"month":           monthStr,
			"redis_available": redisOK,
			"scopes":          scopes,
		})
	}
}

func parseTimeParam(s string, defaultVal time.Time) time.Time {
	if s == "" {
		return defaultVal
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return defaultVal
	}
	return t
}

func validateUsageReportRange(from, to time.Time, granularity string) error {
	if !from.Before(to) {
		return fmt.Errorf("from must be before to")
	}

	maxRange := usageReportMaxRange(granularity)
	if maxRange <= 0 {
		return nil
	}
	if to.Sub(from) > maxRange {
		return fmt.Errorf("%s usage reports are limited to %s", granularity, maxRange)
	}
	return nil
}

func usageReportMaxRange(granularity string) time.Duration {
	switch granularity {
	case "hourly":
		return 31 * 24 * time.Hour
	case "daily", "":
		return 366 * 24 * time.Hour
	case "monthly":
		return 5 * 366 * 24 * time.Hour
	default:
		return 0
	}
}

func modelCatalogHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ProviderStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "provider store is not configured")
			return
		}
		registry := providers.NewRegistry()
		loader := providers.NewConfigLoader(registry)
		loader.SetStore(deps.ProviderStore)
		if err := loader.LoadFromDB(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "list_provider_models", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, proxymodels.BuildResponseFromProviders(registry.ListProviders()))
	}
}

func listProviderCatalogsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.ProviderStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "provider store is not configured")
			return
		}
		catalogs, err := deps.ProviderStore.ListProviderCatalogs(r.Context(), "pi")
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_provider_catalogs", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"provider_catalogs": catalogs})
	}
}

// OnboardingResponse holds copy-paste ready Pi configuration instructions.
type OnboardingResponse struct {
	Install    string `json:"install"`
	Extension  string `json:"extension"`
	ModelsJSON string `json:"models_json"`
	CurlTest   string `json:"curl_test"`
}

// installInstructions renders the preferred onboarding artifact: one-line
// installers that download the user's personalized extension straight from
// the proxy (key + base URL baked in by the download endpoint). Nothing else
// needs to be sent — auto-update keeps the installed copy current.
func installInstructions(baseURL, key string) string {
	return fmt.Sprintf(`# Install the corp-proxy Pi extension — pick your platform, paste, run.
# The download is personalized with your API key; auto-update keeps it current.
# Afterwards: restart pi and select a model marked "(corp)".

# macOS / Linux:
mkdir -p ~/.pi/agent/extensions && curl -fsSL -H "Authorization: Bearer %[2]s" %[1]s/proxy/v1/extension-download -o ~/.pi/agent/extensions/corp-proxy.ts && echo "corp-proxy installed — restart pi"

# Windows (PowerShell):
ni -ItemType Directory -Force "$env:USERPROFILE\.pi\agent\extensions" | Out-Null; iwr -Headers @{Authorization="Bearer %[2]s"} %[1]s/proxy/v1/extension-download -OutFile "$env:USERPROFILE\.pi\agent\extensions\corp-proxy.ts"; "corp-proxy installed — restart pi"
`, baseURL, key)
}

// personalizedExtension returns the real corp-proxy extension source with the
// user's key and this deployment's base URL baked in — the same artifact the
// auto-update download serves, so onboarding can never drift behind it.
func personalizedExtension(baseURL, key string) string {
	source, err := extensionversion.LoadSource()
	if err != nil {
		return fmt.Sprintf(`// Extension source unavailable on this server (%v).
// Download it instead:
//   mkdir -p ~/.pi/agent/extensions
//   curl -H "Authorization: Bearer %s" %s/proxy/v1/extension-download \
//     -o ~/.pi/agent/extensions/corp-proxy.ts`, err, key, baseURL)
	}
	header := "// Save as ~/.pi/agent/extensions/corp-proxy.ts\n" +
		"//   mkdir -p ~/.pi/agent/extensions\n" +
		"// Then restart pi. Your API key is embedded below; auto-update keeps it.\n\n"
	return header + extensionversion.Personalize(source, key, baseURL)
}

// generateOnboarding builds copy-paste ready Pi onboarding instructions.
func generateOnboarding(baseURL, key string) OnboardingResponse {
	// Strip trailing slash from baseURL for consistent formatting.
	baseURL = strings.TrimRight(baseURL, "/")

	// models.json is generated from the proxy's canonical model catalog
	// (internal/httpapi/proxymodels) so it can never drift from what the
	// /proxy/v1/models endpoint and the extension serve.
	modelsJSON, err := proxymodels.PiModelsJSON(baseURL, key)
	if err != nil {
		modelsJSON = "// failed to generate models.json: " + err.Error()
	}

	return OnboardingResponse{
		Install:   installInstructions(baseURL, key),
		Extension: personalizedExtension(baseURL, key),
		ModelsJSON: fmt.Sprintf(`// Save as ~/.pi/agent/models.json
//
// Email this to the user:
//
// 1. Install Pi:
//    pnpm add -g --ignore-scripts @earendil-works/pi-coding-agent
//
//    If you do not use pnpm, follow the install command in the Pi quickstart:
//    https://pi.dev/docs/latest/quickstart
//
// 2. Create this folder if it does not already exist:
//    mkdir -p ~/.pi/agent
// 3. Save the JSON below as:
//    ~/.pi/agent/models.json
// 4. Start Pi in your project directory:
//    cd /path/to/project
//    pi
// 5. Choose DeepSeek V4 Pro or DeepSeek V4 Flash with /model, then send a
//    short test prompt.
//
// Useful first places to start:
// - Quickstart: https://pi.dev/docs/latest/quickstart
// - Using Pi: https://pi.dev/docs/latest/using-pi
// - Custom models: https://pi.dev/docs/latest/custom-models
//
// This routes requests through the corporate proxy. The embedded key is
// private to this user and should not be forwarded outside the setup email.

%s`, modelsJSON),
		CurlTest: fmt.Sprintf(`# Verify the proxy is healthy:
curl -s %[1]s/healthz

# Test DeepSeek V4 Pro with thinking mode:
curl -s %[1]s/deepseek/v1/chat/completions \\
  -H "Authorization: Bearer %[2]s" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"Think step by step: 37 * 84"}],"reasoning_effort":"high"}'

# Test DeepSeek V4 Flash:
curl -s %[1]s/deepseek/v1/chat/completions \\
  -H "Authorization: Bearer %[2]s" \\
  -H "Content-Type: application/json" \\
  -d '{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"Hello"}]}'
`, baseURL, key),
	}
}

// onboardingHandler generates Pi onboarding instructions for a provided key.
// This is useful for re-generating or distributing onboarding for existing keys.
// POST /tenants/{tenantID}/users/{userID}/onboarding
// Body: { "key": "cpk_live_..." }
func onboardingHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.PublicBaseURL == "" {
			writeError(w, http.StatusBadRequest, "not_configured", "PROXY_PUBLIC_BASE_URL is not set; cannot generate onboarding")
			return
		}

		var req struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if req.Key == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "key is required")
			return
		}

		onboarding := generateOnboarding(deps.PublicBaseURL, req.Key)
		writeJSON(w, http.StatusOK, onboarding)
	}
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]interface{}{
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// Compile-time check that context.Context is used.
var _ context.Context

// --- Session Submissions ---

func listSessionSubmissionsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.SessionSubmissionStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "session submission store not configured")
			return
		}

		q := r.URL.Query()
		tenantID := q.Get("tenant_id")
		limit := 50
		if l := q.Get("limit"); l != "" {
			if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 100 {
				limit = n
			}
		}
		offset := 0
		if o := q.Get("offset"); o != "" {
			if n, err := strconv.Atoi(o); err == nil && n >= 0 {
				offset = n
			}
		}

		total, records, err := deps.SessionSubmissionStore.List(r.Context(), tenantID, limit, offset)
		if err != nil {
			deps.Logger.Error().Err(err).
				Str("tenant_id", tenantID).
				Msg("session submissions list failed")
			writeError(w, http.StatusInternalServerError, "list_session_submissions", err.Error())
			return
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"session_submissions": records,
			"total_count":         total,
		})
	}
}
