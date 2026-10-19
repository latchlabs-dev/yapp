// Package observations provides the public endpoint agents use to record
// project lessons, mistakes, and decisions into duffel mid-session.
package observations

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/memory"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type projectLookup interface {
	GetForTenant(ctx context.Context, tenantID, projectID string) (*pgstore.MemoryProject, error)
}

// Observation is the JSON payload agents POST.
type Observation struct {
	Project   string `json:"project"`
	ProjectID string `json:"project_id,omitempty"`
	Kind      string `json:"kind"`    // mistake | lesson | decision | context
	Content   string `json:"content"` // short, durable note
}

var validKinds = map[string]bool{
	"mistake": true, "lesson": true, "decision": true, "context": true,
}

// NewHandler returns the handler for POST /proxy/v1/observations.
func NewHandler(client *duffel.Client, projects projectLookup, maxContentBytes int, logger zerolog.Logger) http.HandlerFunc {
	if maxContentBytes <= 0 {
		maxContentBytes = 4096
	}

	return func(w http.ResponseWriter, r *http.Request) {
		var obs Observation
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(maxContentBytes)+1024)).Decode(&obs); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "failed to parse observation: "+err.Error())
			return
		}

		project, err := memory.SanitizeProject(obs.Project)
		if err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", "invalid project: "+err.Error())
			return
		}
		authCtx := middleware.GetAuthContext(r.Context())
		if authCtx == nil || authCtx.TenantID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authenticated tenant required")
			return
		}
		var scopedProject string
		if obs.ProjectID != "" {
			if projects == nil {
				writeError(w, http.StatusServiceUnavailable, "unavailable", "project resolution unavailable")
				return
			}
			resolved, resolveErr := projects.GetForTenant(r.Context(), authCtx.TenantID, obs.ProjectID)
			if resolveErr != nil || resolved == nil {
				writeError(w, http.StatusBadRequest, "validation_error", "unknown project_id")
				return
			}
			project = resolved.Slug
			scopedProject, err = memory.ScopedProject(authCtx.TenantID, resolved.ID)
			if err != nil {
				writeError(w, http.StatusBadRequest, "validation_error", "invalid project scope")
				return
			}
		}

		obs.Kind = strings.ToLower(strings.TrimSpace(obs.Kind))
		if !validKinds[obs.Kind] {
			writeError(w, http.StatusBadRequest, "validation_error", "kind must be one of: mistake, lesson, decision, context")
			return
		}

		obs.Content = strings.TrimSpace(obs.Content)
		if obs.Content == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "content is required")
			return
		}
		if len(obs.Content) > maxContentBytes {
			writeError(w, http.StatusBadRequest, "validation_error", "content exceeds maximum size")
			return
		}

		// A checkout label is not repository identity. Preserve unresolved writes
		// outside projects/ rather than rejecting them or guessing a shared owner.
		if scopedProject == "" {
			saveToPurgatory(w, r, client, obs, project, authCtx, logger)
			return
		}

		source := "source: agent"
		if authCtx.UserID != "" {
			source += ", user: " + authCtx.UserID
		}
		entry := "**[" + obs.Kind + "]** (" + source + ")\n\n" + obs.Content
		unlock := memory.LockProject(scopedProject)
		defer unlock()
		if err := client.JournalAppend(r.Context(), memory.LessonsFilePath(scopedProject), entry); err != nil {
			logger.Error().Err(err).Str("project", scopedProject).Msg("failed to record observation")
			writeError(w, http.StatusServiceUnavailable, "unavailable", "memory backend is unavailable")
			return
		}

		logger.Info().Str("project", scopedProject).Str("kind", obs.Kind).Msg("observation recorded")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "recorded", "project": project})
	}
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]map[string]string{
		"error": {"type": errType, "message": message},
	})
}
