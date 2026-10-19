// Package sessionsubmit provides the public endpoint for Pi extension session
// submission. The corp-proxy extension sends Pi session metadata at
// session_shutdown so the server can correlate it with conversation_payloads
// and usage_events.
package sessionsubmit

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/hooks"
	"github.com/earlgrey/piproxy/internal/httpapi/dataplane"
	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/memory"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// Submission is the JSON payload the extension sends.
type Submission struct {
	ConversationID   string   `json:"conversation_id"`
	SessionFile      string   `json:"session_file,omitempty"`
	MessageCount     int      `json:"message_count"`
	Model            string   `json:"model,omitempty"`
	TotalTokens      int64    `json:"total_tokens,omitempty"`
	TotalCostCents   float64  `json:"total_cost_cents,omitempty"`
	StartedAt        string   `json:"started_at,omitempty"` // RFC3339
	EndedAt          string   `json:"ended_at,omitempty"`   // RFC3339
	ExtensionVersion string   `json:"extension_version,omitempty"`
	PiVersion        string   `json:"pi_version,omitempty"`
	ThinkingLevel    string   `json:"thinking_level,omitempty"` // off|minimal|low|medium|high|xhigh|max
	SessionName      string   `json:"session_name,omitempty"`   // human title set via /name
	FirstPrompt      string   `json:"first_prompt,omitempty"`   // truncated first user prompt
	RunMode          string   `json:"run_mode,omitempty"`       // tui|rpc|json|print
	Summary          string   `json:"summary,omitempty"`        // free-text session summary
	SystemPrompt     string   `json:"system_prompt,omitempty"`  // captured system prompt
	Project          string   `json:"project,omitempty"`        // project identifier for memory distillation
	ProjectID        string   `json:"project_id,omitempty"`
	Repo             string   `json:"repo,omitempty"`
	Remotes          []string `json:"remotes,omitempty"`
}

type SessionStore interface {
	Put(ctx context.Context, record *pgstore.SessionSubmissionRecord, summaryJSON []byte) error
}

type projectResolver interface {
	Resolve(ctx context.Context, tenantID, slug string, remotes []string) (*pgstore.MemoryProject, error)
	GetForTenant(ctx context.Context, tenantID, projectID string) (*pgstore.MemoryProject, error)
}

// NewHandler returns an HTTP handler for POST /proxy/v1/session-submit.
func NewHandler(store SessionStore, projects projectResolver, hookManager *hooks.Manager, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]map[string]string{
				"error": {"type": "method_not_allowed", "message": "POST required"},
			})
			return
		}

		var sub Submission
		if err := json.NewDecoder(r.Body).Decode(&sub); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", "failed to parse submission body: "+err.Error())
			return
		}
		sub.ConversationID = capture.NormalizeConversationID(sub.ConversationID)

		if sub.ConversationID == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "conversation_id is required")
			return
		}

		// Build the store record.
		authCtx := middleware.GetAuthContext(r.Context())
		if authCtx == nil || authCtx.TenantID == "" || authCtx.KeyID == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "authenticated session owner required")
			return
		}
		ownerID := "key:" + authCtx.KeyID
		if authCtx.UserID != "" {
			ownerID = "user:" + authCtx.UserID
		}
		sessionFile := strings.ReplaceAll(strings.TrimSpace(sub.SessionFile), `\`, "/")
		if sessionFile != "" {
			sessionFile = filepath.Base(sessionFile)
		}
		record := &pgstore.SessionSubmissionRecord{
			TenantID:         authCtx.TenantID,
			UserID:           authCtx.UserID,
			KeyID:            authCtx.KeyID,
			OwnerID:          ownerID,
			ConversationID:   sub.ConversationID,
			SessionFile:      sessionFile,
			MessageCount:     sub.MessageCount,
			Model:            sub.Model,
			TotalTokens:      sub.TotalTokens,
			TotalCostCents:   sub.TotalCostCents,
			ExtensionVersion: sub.ExtensionVersion,
			PiVersion:        sub.PiVersion,
			ThinkingLevel:    normalizeAttribution(sub.ThinkingLevel, 16, validThinkingLevels),
			SessionName:      normalizeAttribution(sub.SessionName, 200, nil),
			FirstPrompt:      normalizeAttribution(sub.FirstPrompt, 300, nil),
			RunMode:          normalizeAttribution(sub.RunMode, 8, validRunModes),
		}

		// Project is optional; an unusable name is dropped rather than rejected
		// so older/misconfigured extensions never lose stat submissions.
		if sub.Project != "" {
			if project, err := memory.SanitizeProject(sub.Project); err == nil {
				record.Project = project
			}
		}
		if projects != nil {
			var resolved *pgstore.MemoryProject
			var resolveErr error
			if sub.ProjectID != "" {
				resolved, resolveErr = projects.GetForTenant(r.Context(), authCtx.TenantID, sub.ProjectID)
			} else {
				remotes := append([]string(nil), sub.Remotes...)
				if sub.Repo != "" {
					remotes = append(remotes, sub.Repo)
				}
				resolved, resolveErr = projects.Resolve(r.Context(), authCtx.TenantID, record.Project, remotes)
			}
			if resolveErr != nil {
				writeError(w, http.StatusBadRequest, "validation_error", "invalid project identity")
				return
			}
			if resolved != nil {
				record.ProjectID = resolved.ID
				record.Project = resolved.Slug
			}
		}

		if sub.StartedAt != "" {
			if t, err := time.Parse(time.RFC3339, sub.StartedAt); err == nil {
				record.StartedAt = &t
			}
		}
		if sub.EndedAt != "" {
			if t, err := time.Parse(time.RFC3339, sub.EndedAt); err == nil {
				record.EndedAt = &t
			}
		}

		// Build a structured summary for storage.
		summaryMap := map[string]interface{}{
			"summary":       sub.Summary,
			"system_prompt": sub.SystemPrompt,
		}
		summaryJSON, _ := json.Marshal(summaryMap)

		if err := store.Put(r.Context(), record, summaryJSON); err != nil {
			logger.Error().Err(err).
				Str("conversation_id", sub.ConversationID).
				Msg("failed to store session submission")
			writeError(w, http.StatusInternalServerError, "storage_error", "failed to store session submission")
			return
		}
		dataplane.EmitSessionUploadedHook(r, hookManager, record.ConversationID, record.SessionFile, record.Project, record.ProjectID, record.Model, record.MessageCount, record.TotalTokens, record.TotalCostCents, record.StartedAt, record.EndedAt)

		logger.Info().
			Str("conversation_id", sub.ConversationID).
			Int("message_count", sub.MessageCount).
			Str("model", sub.Model).
			Msg("session submission stored")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":          "accepted",
			"conversation_id": sub.ConversationID,
		})
	}
}

var (
	validThinkingLevels = map[string]bool{
		"off": true, "minimal": true, "low": true, "medium": true,
		"high": true, "xhigh": true, "max": true,
	}
	validRunModes = map[string]bool{"tui": true, "rpc": true, "json": true, "print": true}
)

// normalizeAttribution trims and caps a caller-supplied session attribution
// field. Enum-constrained fields (thinking level, run mode) are dropped when
// they hold an unknown value so junk never reaches the admin UI.
func normalizeAttribution(value string, maxRunes int, allowed map[string]bool) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if allowed != nil {
		value = strings.ToLower(value)
		if !allowed[value] {
			return ""
		}
	}
	if runes := []rune(value); len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return value
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}
