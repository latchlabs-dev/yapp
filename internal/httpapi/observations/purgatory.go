package observations

import (
	"encoding/json"
	"net/http"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/auth"
	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/memory"
)

// saveToPurgatory durably appends an unattributed observation with enough
// provenance for later review. Nothing promotes it by checkout basename, even
// if a repository with the same label is subsequently resolved.
func saveToPurgatory(w http.ResponseWriter, r *http.Request, client *duffel.Client, obs Observation, project string, authCtx *auth.Context, logger zerolog.Logger) {
	filePath, err := memory.PurgatoryFilePath(authCtx.TenantID, authCtx.UserID, authCtx.KeyID)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "invalid purgatory scope")
		return
	}
	const reason = "unresolved repository identity"
	// JSON-encode caller-provided labels so they cannot forge provenance lines.
	provenance, _ := json.Marshal(struct {
		Reason    string `json:"reason"`
		Project   string `json:"project"`
		TenantID  string `json:"tenant_id"`
		UserID    string `json:"user_id,omitempty"`
		KeyID     string `json:"key_id,omitempty"`
		RequestID string `json:"request_id,omitempty"`
	}{
		Reason: reason, Project: obs.Project, TenantID: authCtx.TenantID,
		UserID: authCtx.UserID, KeyID: authCtx.KeyID,
		RequestID: middleware.GetRequestID(r.Context()),
	})
	entry := "**[" + obs.Kind + "]** (source: agent, status: purgatory)\n\n" +
		"```json\n" + string(provenance) + "\n```\n\n" + obs.Content
	unlock := memory.LockProject(filePath)
	defer unlock()
	if err := client.JournalAppend(r.Context(), filePath, entry); err != nil {
		logger.Error().Err(err).Str("project", project).Msg("failed to save observation in purgatory")
		writeError(w, http.StatusServiceUnavailable, "unavailable", "memory backend is unavailable")
		return
	}

	logger.Info().
		Str("request_id", middleware.GetRequestID(r.Context())).
		Str("tenant_id", authCtx.TenantID).
		Str("user_id", authCtx.UserID).
		Str("key_id", authCtx.KeyID).
		Str("project", project).
		Str("kind", obs.Kind).
		Str("reason", reason).
		Msg("observation saved in purgatory")

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status": "purgatory", "project": project, "reason": reason,
	})
}
