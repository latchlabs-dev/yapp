package admin

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/earlgrey/piproxy/internal/auth"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// Web-console access control. Nobody can sign into the console until a domain
// is mapped here: `users` is UNIQUE (tenant_id, email), so an email address
// alone cannot identify a tenant, and the login flow refuses rather than
// guessing one. This is the endpoint that makes the console usable at all.

// defaultWebScopes is what a console session gets unless a domain overrides it.
// Deliberately excludes the session scopes: AGENTS.md §29 keeps session and
// memory grants separate, and a browser session has no Pi session to submit.
var defaultWebScopes = []string{
	auth.ScopeLLMInvoke,
	auth.ScopeLLMModelsRead,
	auth.ScopeMemoryContextRead,
	auth.ScopeMemoryObserve,
}

type webDomainInput struct {
	Domain        string   `json:"domain"`
	TenantID      string   `json:"tenant_id"`
	DefaultScopes []string `json:"default_scopes"`
	Status        string   `json:"status"`
}

func requireWebIdentityStore(deps *Dependencies, w http.ResponseWriter) bool {
	if deps.WebIdentityStore == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "web console access control is not configured")
		return false
	}
	return true
}

func listWebDomainsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireWebIdentityStore(deps, w) {
			return
		}
		domains, err := deps.WebIdentityStore.ListDomains(r.Context())
		if err != nil {
			deps.Logger.Error().Err(err).Msg("web domains list failed")
			writeError(w, http.StatusInternalServerError, "list_web_domains", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"domains":        domains,
			"default_scopes": defaultWebScopes,
		})
	}
}

func upsertWebDomainHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireWebIdentityStore(deps, w) {
			return
		}
		var input webDomainInput
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&input); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}

		domain := pgstore.NormalizeDomain(input.Domain)
		if domain == "" || !strings.Contains(domain, ".") {
			writeError(w, http.StatusBadRequest, "validation_error", "a valid email domain is required")
			return
		}
		if strings.TrimSpace(input.TenantID) == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "tenant_id is required")
			return
		}
		// Verify the tenant exists before mapping to it. A typo here would
		// otherwise be discovered by a user hitting a foreign-key error mid-login.
		if deps.TenantStore != nil {
			if _, err := deps.TenantStore.GetByID(r.Context(), input.TenantID); err != nil {
				writeError(w, http.StatusBadRequest, "validation_error", "tenant "+input.TenantID+" not found")
				return
			}
		}

		scopes := input.DefaultScopes
		if len(scopes) == 0 {
			scopes = defaultWebScopes
		}
		for _, scope := range scopes {
			if strings.HasPrefix(scope, "admin:") {
				writeError(w, http.StatusBadRequest, "validation_error",
					"browser sessions may not carry admin scopes")
				return
			}
		}
		status := input.Status
		if status == "" {
			status = "active"
		}

		record := &pgstore.WebIdentityDomain{
			Domain:        domain,
			TenantID:      input.TenantID,
			DefaultScopes: scopes,
			Status:        status,
		}
		if err := deps.WebIdentityStore.UpsertDomain(r.Context(), record); err != nil {
			deps.Logger.Error().Err(err).Str("domain", domain).Msg("web domain upsert failed")
			writeError(w, http.StatusInternalServerError, "upsert_web_domain", err.Error())
			return
		}
		deps.Logger.Info().Str("domain", domain).Str("tenant_id", input.TenantID).
			Msg("web console domain mapped")
		writeJSON(w, http.StatusOK, record)
	}
}

// deleteWebDomainHandler unmaps a domain. Live sessions are unaffected: they
// carry the scopes they were minted with and expire on their own schedule.
// Removing a mapping only stops *new* sign-ins from that domain.
func deleteWebDomainHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireWebIdentityStore(deps, w) {
			return
		}
		domain := pgstore.NormalizeDomain(chi.URLParam(r, "domain"))
		if domain == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "domain is required")
			return
		}
		if err := deps.WebIdentityStore.DeleteDomain(r.Context(), domain); err != nil {
			writeError(w, http.StatusInternalServerError, "delete_web_domain", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status": "deleted",
			"note":   "existing browser sessions keep working until they expire; revoke the user to cut them off now",
		})
	}
}

// listWebIdentitiesHandler shows who has actually signed in, which is how an
// admin confirms a new domain mapping worked.
func listWebIdentitiesHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireWebIdentityStore(deps, w) {
			return
		}
		identities, err := deps.WebIdentityStore.ListIdentities(r.Context(), r.URL.Query().Get("tenant_id"), 200)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_web_identities", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"identities": identities})
	}
}

// revokeWebSessionsHandler cuts off a user's live browser sessions immediately,
// for when unmapping a domain is not fast enough.
func revokeWebSessionsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.WebSessionStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "web console sessions are not configured")
			return
		}
		userID := strings.TrimSpace(chi.URLParam(r, "userID"))
		if userID == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "userID is required")
			return
		}
		if err := deps.WebSessionStore.RevokeAllForUser(r.Context(), userID); err != nil {
			writeError(w, http.StatusInternalServerError, "revoke_web_sessions", err.Error())
			return
		}
		deps.Logger.Info().Str("user_id", userID).Msg("web console sessions revoked")
		writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
	}
}
