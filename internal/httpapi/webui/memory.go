package webui

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/memory"
)

// maxMemoryDocBytes caps a user-authored memory.md. The compactor rewrites this
// document from an LLM, so an unbounded editor would let a console user plant
// far more into every future agent context than any distiller ever would.
const maxMemoryDocBytes = 256 * 1024

// maxLessonBytes matches the observation cap applied to agent-recorded memory.
const maxLessonBytes = 4096

// maxSearchQueryBytes bounds the memory-search query. The search backend is a
// shared service; an unbounded query string is a cheap way to make it work
// hard for one tenant.
const maxSearchQueryBytes = 512

// scopedProjectFor resolves the duffel storage path for a project the caller's
// tenant owns. Every memory route goes through it, so a console user can only
// ever address their own tenant's memory directory.
func (deps *Dependencies) scopedProjectFor(ctx context.Context, tenantID, projectID string) (string, error) {
	project, err := deps.resolveTenantProject(ctx, tenantID, projectID)
	if err != nil {
		return "", err
	}
	return memory.ScopedProject(tenantID, project.ID)
}

// requireMemory guards the routes that need duffel, reporting the same
// service-unavailable shape the data plane uses when memory is switched off.
func (deps *Dependencies) requireMemory(w http.ResponseWriter) bool {
	if deps.Duffel == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "project memory is not configured")
		return false
	}
	return true
}

// memoryProject resolves the caller, the project, and the scoped duffel path in
// one step, writing the response on failure.
func (deps *Dependencies) memoryProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	authCtx, ok := requireAuth(w, r)
	if !ok {
		return "", false
	}
	scoped, err := deps.scopedProjectFor(r.Context(), authCtx.TenantID, chi.URLParam(r, "projectID"))
	if err != nil {
		writeError(w, http.StatusNotFound, "project_not_found", "project not found")
		return "", false
	}
	return scoped, true
}

// getProjectMemoryHandler returns the curated memory document and recent
// lessons for a project.
//
// A duffel outage is reported as degraded rather than as an error: the console
// must show the user that memory is unavailable, because the alternative —
// rendering an empty document as though it were the real one — is how a silent
// degradation gets mistaken for "this project has no memory".
func getProjectMemoryHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.requireMemory(w) {
			return
		}
		scoped, ok := deps.memoryProject(w, r)
		if !ok {
			return
		}

		doc, err := deps.Duffel.ReadFile(r.Context(), memory.FilePath(scoped))
		if err != nil && !errors.Is(err, duffel.ErrNotFound) {
			deps.Logger.Warn().Err(err).Str("project", scoped).Msg("web memory read failed")
			writeJSON(w, http.StatusOK, map[string]any{
				"memory":          "",
				"lessons":         []any{},
				"degraded":        true,
				"degraded_reason": "project memory is temporarily unreachable",
			})
			return
		}

		lessonsDoc, err := deps.Duffel.ReadFile(r.Context(), memory.LessonsFilePath(scoped))
		if err != nil && !errors.Is(err, duffel.ErrNotFound) {
			deps.Logger.Warn().Err(err).Str("project", scoped).Msg("web lessons read failed")
		}

		entries := memory.ParseJournalEntries(lessonsDoc)
		lessonsCapped := len(entries) > 50
		if lessonsCapped {
			entries = entries[len(entries)-50:]
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"memory":         doc,
			"memory_bytes":   len(doc),
			"memory_tokens":  memory.EstimateTokens(doc),
			"lessons":        entries,
			"degraded":       false,
			"lessons_capped": lessonsCapped,
		})
	}
}

// putProjectMemoryHandler replaces the curated memory document.
//
// This writes the same file the off-peak compactor owns. There is no
// cross-process lock (the proxy and admin share the workspace), so a save that
// races a compaction can be overwritten; the console surfaces the saved
// content back so the user can see what stuck.
func putProjectMemoryHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.requireMemory(w) {
			return
		}
		scoped, ok := deps.memoryProject(w, r)
		if !ok {
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			return
		}
		if strings.TrimSpace(body.Content) == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "content is required")
			return
		}
		if len(body.Content) > maxMemoryDocBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large",
				"memory document exceeds "+strconv.Itoa(maxMemoryDocBytes/1024)+" KB")
			return
		}

		unlock := memory.LockProject(scoped)
		defer unlock()
		if err := deps.Duffel.WriteFile(r.Context(), memory.FilePath(scoped), body.Content); err != nil {
			deps.Logger.Error().Err(err).Str("project", scoped).Msg("web memory write failed")
			writeError(w, http.StatusBadGateway, "duffel_error", "could not save project memory")
			return
		}
		deps.Logger.Info().Str("project", scoped).Msg("memory.md edited via web console")
		writeJSON(w, http.StatusOK, map[string]any{
			"status":        "saved",
			"memory_tokens": memory.EstimateTokens(body.Content),
		})
	}
}

// appendProjectLessonHandler appends a user-authored entry to the lessons
// journal, attributed in the same "(source: …, user: …)" form the admin
// projects view already parses, so web-authored lessons render with attribution
// alongside agent- and distiller-authored ones.
func appendProjectLessonHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.requireMemory(w) {
			return
		}
		authCtx, ok := requireAuth(w, r)
		if !ok {
			return
		}
		scoped, err := deps.scopedProjectFor(r.Context(), authCtx.TenantID, chi.URLParam(r, "projectID"))
		if err != nil {
			writeError(w, http.StatusNotFound, "project_not_found", "project not found")
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		if err := decodeJSON(w, r, &body); err != nil {
			return
		}
		content := strings.TrimSpace(body.Content)
		if content == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "content is required")
			return
		}
		if len(content) > maxLessonBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "lesson exceeds 4 KB")
			return
		}

		entry := content + "\n\n(source: web, user: " + authCtx.UserID + ")"
		unlock := memory.LockProject(scoped)
		defer unlock()
		if err := deps.Duffel.JournalAppend(r.Context(), memory.LessonsFilePath(scoped), entry); err != nil {
			deps.Logger.Error().Err(err).Str("project", scoped).Msg("web lesson append failed")
			writeError(w, http.StatusBadGateway, "duffel_error", "could not record lesson")
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded"})
	}
}

// projectBundleHandler returns the exact bundle a turn in this project would
// receive. It is the ground truth for "what does the model actually see", and
// the only reliable way to catch memory that has silently degraded to empty.
func projectBundleHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.MemoryBuilder == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "context assembly is not configured")
			return
		}
		scoped, ok := deps.memoryProject(w, r)
		if !ok {
			return
		}
		budget := deps.Config.MemoryBudgetTokens
		if raw := r.URL.Query().Get("budget"); raw != "" {
			if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 && parsed <= 32000 {
				budget = parsed
			}
		}

		bundle := deps.MemoryBuilder.Build(r.Context(), scoped, r.URL.Query().Get("hint"), budget, false)
		writeJSON(w, http.StatusOK, map[string]any{
			"bundle":          bundle.Markdown,
			"token_estimate":  bundle.TokenEstimate,
			"degraded":        bundle.Degraded,
			"degraded_reason": bundle.DegradedReason,
			"cached":          bundle.FromCache,
		})
	}
}

// tenantSearchPrefix is the workspace path prefix that scopes a search to one
// tenant. It must go through SanitizeProject: tenant IDs carry uppercase ULID
// characters, while Duffel directory names are lowercased when they are
// created, and a raw prefix silently matches nothing.
func tenantSearchPrefix(tenantID string) (string, error) {
	tenant, err := memory.SanitizeProject(tenantID)
	if err != nil {
		return "", err
	}
	return "projects/" + tenant + "--", nil
}

// filterTenantResults drops hits outside the tenant's directory. Results from
// other tenants are filtered here as a second boundary after scoped Duffel
// search. Reject noncanonical paths rather than accepting index ambiguity.
func filterTenantResults(results []duffel.SearchResult, prefix string) []duffel.SearchResult {
	tenant := strings.TrimSuffix(strings.TrimPrefix(prefix, "projects/"), "--")
	filtered := make([]duffel.SearchResult, 0, len(results))
	for _, res := range results {
		if duffel.TenantPathMatches(res.Path, tenant) {
			filtered = append(filtered, res)
		}
	}
	return filtered
}

// searchMemoryHandler searches the caller's tenant memory.
func searchMemoryHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !deps.requireMemory(w) {
			return
		}
		authCtx, ok := requireAuth(w, r)
		if !ok {
			return
		}
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		if query == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "q is required")
			return
		}
		if len(query) > maxSearchQueryBytes {
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", "query is too long")
			return
		}

		tenant, err := memory.SanitizeProject(authCtx.TenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "scope_failed", "could not scope the search to your tenant")
			return
		}
		results, err := deps.Duffel.Search(r.Context(), query, duffel.SearchOpts{Limit: 25, TenantScope: tenant})
		if err != nil {
			deps.Logger.Warn().Err(err).Msg("web memory search failed")
			writeJSON(w, http.StatusOK, map[string]any{"results": []any{}, "degraded": true})
			return
		}

		prefix, err := tenantSearchPrefix(authCtx.TenantID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "scope_failed", "could not scope the search to your tenant")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"results": filterTenantResults(results, prefix), "degraded": false})
	}
}
