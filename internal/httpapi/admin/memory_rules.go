package admin

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/earlgrey/piproxy/internal/memory"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// memoryRuleRequest is the admin payload for creating/updating an injection rule.
type memoryRuleRequest struct {
	ID           string `json:"id,omitempty"`            // set to update a specific rule
	Project      string `json:"project,omitempty"`       // empty = any project
	TenantID     string `json:"tenant_id,omitempty"`     // empty = any tenant
	UserID       string `json:"user_id,omitempty"`       // empty = any user
	Enabled      *bool  `json:"enabled"`                 // required: inject context when matched?
	BudgetTokens *int   `json:"budget_tokens,omitempty"` // optional bundle budget override
	Note         string `json:"note,omitempty"`
}

// listMemoryRulesHandler handles GET /memory-rules.
func listMemoryRulesHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.MemoryRuleStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "memory rules are not configured")
			return
		}
		rules, err := deps.MemoryRuleStore.List(r.Context())
		if err != nil {
			deps.Logger.Error().Err(err).Msg("failed to list memory rules")
			writeError(w, http.StatusInternalServerError, "storage_error", "failed to list memory rules")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"memory_rules": rules})
	}
}

// upsertMemoryRuleHandler handles POST /memory-rules.
func upsertMemoryRuleHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.MemoryRuleStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "memory rules are not configured")
			return
		}

		var req memoryRuleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "failed to parse rule: "+err.Error())
			return
		}
		if req.Enabled == nil {
			writeError(w, http.StatusBadRequest, "validation_error", "enabled is required")
			return
		}
		if req.Project != "" {
			project, err := memory.SanitizeProject(req.Project)
			if err != nil {
				writeError(w, http.StatusBadRequest, "validation_error", "invalid project: "+err.Error())
				return
			}
			req.Project = project
		}
		if req.BudgetTokens != nil && *req.BudgetTokens <= 0 {
			writeError(w, http.StatusBadRequest, "validation_error", "budget_tokens must be positive")
			return
		}

		rule, err := deps.MemoryRuleStore.Upsert(r.Context(), &pgstore.MemoryInjectionRule{
			ID:           req.ID,
			Project:      req.Project,
			TenantID:     req.TenantID,
			UserID:       req.UserID,
			Enabled:      *req.Enabled,
			BudgetTokens: req.BudgetTokens,
			Note:         req.Note,
		})
		if err != nil {
			deps.Logger.Error().Err(err).Msg("failed to upsert memory rule")
			writeError(w, http.StatusInternalServerError, "storage_error", "failed to store memory rule")
			return
		}

		deps.Logger.Info().
			Str("rule_id", rule.ID).
			Str("project", rule.Project).
			Bool("enabled", rule.Enabled).
			Msg("memory injection rule upserted")
		writeJSON(w, http.StatusOK, map[string]any{"memory_rule": rule})
	}
}

// deleteMemoryRuleHandler handles DELETE /memory-rules/{ruleID}.
func deleteMemoryRuleHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.MemoryRuleStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "memory rules are not configured")
			return
		}
		ruleID := chi.URLParam(r, "ruleID")
		if err := deps.MemoryRuleStore.Delete(r.Context(), ruleID); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "memory rule not found")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "id": ruleID})
	}
}
