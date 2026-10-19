// Package memorycontext provides the public endpoint the corp-proxy extension
// calls at session start to fetch a duffel-backed project context bundle.
package memorycontext

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/httpapi/middleware"
	"github.com/earlgrey/piproxy/internal/memory"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// Response is the JSON payload returned to the extension.
type Response struct {
	Project        string    `json:"project"`
	ProjectID      string    `json:"project_id,omitempty"`
	Inject         bool      `json:"inject"`
	Reason         string    `json:"reason,omitempty"`
	Bundle         string    `json:"bundle"`
	TokenEstimate  int       `json:"token_estimate"`
	Degraded       bool      `json:"degraded"`
	DegradedReason string    `json:"degraded_reason,omitempty"`
	Cache          string    `json:"cache"` // "hit" | "miss" | "bypass"
	GeneratedAt    time.Time `json:"generated_at"`
}

// Config tunes the context handler.
type Config struct {
	DefaultBudget int // default bundle token budget
}

type projectResolver interface {
	Resolve(ctx context.Context, tenantID, slug string, remotes []string) (*pgstore.MemoryProject, error)
}

// NewHandler serves GET/POST /proxy/v1/context. New clients use a small JSON
// POST body so hints and repository remotes never enter proxy access logs;
// query parameters remain accepted for older extension versions.
//
// The hook chain controls automatic injection. Explicit on-demand retrieval
// may return the same tenant-scoped bundle even when automatic injection is
// disabled. Synthesis remains server-side; this endpoint only reads curated
// memory and recent lessons.
func NewHandler(builder *memory.Builder, hooks *memory.HookChain, bootstrapper *memory.Bootstrapper, projects projectResolver, cfg Config, logger zerolog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		input := struct {
			Project  string   `json:"project"`
			Hint     string   `json:"hint"`
			Repo     string   `json:"repo"`
			Remotes  []string `json:"remotes"`
			Tools    bool     `json:"tools"`
			Retrieve bool     `json:"retrieve"`
		}{
			Project: r.URL.Query().Get("project"),
			Hint:    r.URL.Query().Get("hint"),
			Repo:    r.URL.Query().Get("repo"),
		}
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&input); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]map[string]string{
					"error": {"type": "invalid_request", "message": "invalid context request"},
				})
				return
			}
		}

		project, err := memory.SanitizeProject(input.Project)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]map[string]string{
				"error": {"type": "validation_error", "message": "invalid project: " + err.Error()},
			})
			return
		}

		authCtx := middleware.GetAuthContext(r.Context())
		if authCtx == nil || authCtx.TenantID == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]map[string]string{
				"error": {"type": "unauthorized", "message": "authenticated tenant required"},
			})
			return
		}
		var resolved *pgstore.MemoryProject
		storageProject := project
		if projects != nil {
			remotes := append([]string(nil), input.Remotes...)
			if input.Repo != "" {
				remotes = append(remotes, input.Repo)
			}
			resolved, err = projects.Resolve(r.Context(), authCtx.TenantID, project, remotes)
			if err != nil {
				logger.Warn().Err(err).Str("project", project).Msg("repository project resolution failed")
				writeJSON(w, http.StatusServiceUnavailable, map[string]map[string]string{
					"error": {"type": "unavailable", "message": "project resolution unavailable"},
				})
				return
			}
			if resolved != nil {
				storageProject, err = memory.ScopedProject(authCtx.TenantID, resolved.ID)
				if err != nil {
					writeJSON(w, http.StatusBadRequest, map[string]map[string]string{
						"error": {"type": "validation_error", "message": "invalid project scope"},
					})
					return
				}
			}
		}
		hint := input.Hint

		// First sighting of a project scaffolds its duffel folder. Async with
		// a detached context — the request must not wait on (or be failed by)
		// bootstrap writes.
		if bootstrapper != nil && resolved != nil {
			repoURL := sanitizeRepoURL(input.Repo)
			legacyScopedProject, scopeErr := memory.ScopedProject(authCtx.TenantID, resolved.Slug)
			if scopeErr != nil {
				writeJSON(w, http.StatusBadRequest, map[string]map[string]string{
					"error": {"type": "validation_error", "message": "invalid legacy project scope"},
				})
				return
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				bootstrapper.EnsureCanonical(ctx, storageProject, legacyScopedProject, repoURL)
			}()
		}

		req := &memory.InjectionRequest{Project: project, Hint: hint}
		req.TenantID = authCtx.TenantID
		req.UserID = authCtx.UserID
		req.KeyID = authCtx.KeyID

		decision := hooks.Decide(r.Context(), req, cfg.DefaultBudget)

		resp := Response{
			Project:     project,
			Inject:      decision.Inject,
			Reason:      decision.Reason,
			Cache:       "bypass",
			GeneratedAt: time.Now().UTC(),
		}
		if resolved != nil {
			resp.Project = resolved.Slug
			resp.ProjectID = resolved.ID
		} else if projects != nil {
			resp.Inject = false
			resp.Reason = "unresolved repository identity"
		}

		// Explicit retrieval bypasses automatic-injection policy, but not the
		// authenticated tenant scope or canonical repository resolution.
		if builder != nil && (decision.Inject || input.Retrieve) && (projects == nil || resolved != nil) {
			bundle := builder.Build(r.Context(), storageProject, hint, decision.BudgetTokens, input.Tools)
			resp.Bundle = bundle.Markdown
			resp.TokenEstimate = bundle.TokenEstimate
			resp.Degraded = bundle.Degraded
			resp.DegradedReason = bundle.DegradedReason
			if bundle.FromCache {
				resp.Cache = "hit"
			} else if hint == "" {
				resp.Cache = "miss"
			}
			if input.Retrieve && !decision.Inject {
				resp.Reason = "retrieved on demand"
			}
		}

		logger.Info().
			Str("request_id", middleware.GetRequestID(r.Context())).
			Str("project", project).
			Str("project_id", resp.ProjectID).
			Bool("inject", resp.Inject).
			Bool("retrieve", input.Retrieve).
			Bool("bundle_present", resp.Bundle != "").
			Str("reason", resp.Reason).
			Int("tokens", resp.TokenEstimate).
			Bool("degraded", resp.Degraded).
			Str("cache", resp.Cache).
			Msg("context bundle served")

		writeJSON(w, http.StatusOK, resp)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store, private")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// sanitizeRepoURL keeps the recorded repo URL boring: printable, short, no
// credentials. Anything suspicious is dropped (the field is informational).
func sanitizeRepoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 || strings.ContainsAny(raw, " \t\n\r") {
		return ""
	}
	if before, _, found := strings.Cut(raw, "@"); found && strings.Contains(strings.TrimPrefix(before, "https://"), ":") {
		// user:pass@host — drop the whole URL rather than risk leaking creds.
		// (git@host SSH remotes have no colon before the @ and pass through.)
		return ""
	}
	return raw
}
