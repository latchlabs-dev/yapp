package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/memory"
)

// Memory knowledge browser: read and curate the duffel-backed project memory
// (memory.md + lessons journal) directly from the admin UI. All paths are
// built from the sanitized project name only.

func requireDuffel(deps *Dependencies, w http.ResponseWriter) bool {
	if deps.DuffelClient == nil {
		writeError(w, http.StatusServiceUnavailable, "unavailable", "duffel is not configured on the admin")
		return false
	}
	return true
}

func sanitizedProjectParam(w http.ResponseWriter, r *http.Request) (string, bool) {
	project, err := memory.SanitizeProject(chi.URLParam(r, "project"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", "invalid project: "+err.Error())
		return "", false
	}
	return project, true
}

// projectSummary is the at-a-glance health of one project's memory, computed
// for the projects list so the UI can show stats without N round-trips.
type projectSummary struct {
	Name         string `json:"name"`
	Lessons      int    `json:"lessons"`
	MemoryTokens int    `json:"memory_tokens"`
	MemoryBytes  int    `json:"memory_bytes"`
	HasMemory    bool   `json:"has_memory"`
	LastActivity string `json:"last_activity"` // newest lesson timestamp, if any
}

// listMemoryProjectsHandler handles GET /memory/projects. It returns one
// summary per project (lesson count, curated-memory size, last activity) so
// the admin can gauge each project's memory at a glance.
func listMemoryProjectsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireDuffel(deps, w) {
			return
		}
		entries, err := deps.DuffelClient.ListDir(r.Context(), "projects")
		if err != nil {
			if errors.Is(err, duffel.ErrNotFound) {
				writeJSON(w, http.StatusOK, map[string]any{"projects": []projectSummary{}})
				return
			}
			writeError(w, http.StatusBadGateway, "duffel_error", err.Error())
			return
		}

		summaries := make([]projectSummary, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir || e.Name == ".archive" {
				continue
			}
			project, err := memory.SanitizeProject(e.Name)
			if err != nil {
				continue // skip names we can't safely address
			}
			summaries = append(summaries, summarizeProject(r, deps, project))
		}
		writeJSON(w, http.StatusOK, map[string]any{"projects": summaries})
	}
}

// archiveMemoryProjectHandler handles POST /memory/projects/{project}/archive:
// moves the entire project directory under projects/.archive so it disappears
// from the active browser while remaining recoverable on disk.
func archiveMemoryProjectHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireDuffel(deps, w) {
			return
		}
		project, ok := sanitizedProjectParam(w, r)
		if !ok {
			return
		}

		unlock := memory.LockProject(project)
		defer unlock()
		archivedPath, err := deps.DuffelClient.ArchiveDir(r.Context(), "projects/"+project)
		if err != nil {
			if errors.Is(err, duffel.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "project memory not found")
				return
			}
			writeError(w, http.StatusBadGateway, "duffel_error", err.Error())
			return
		}
		deps.Logger.Info().Str("project", project).Str("archived_path", archivedPath).Msg("project memory archived via admin")
		writeJSON(w, http.StatusOK, map[string]string{
			"status":        "archived",
			"project":       project,
			"archived_path": archivedPath,
		})
	}
}

// summarizeProject reads a project's curated memory and lessons journal to
// compute its summary. Missing files are treated as empty (new project), not
// as errors — the projects list should never fail because one project is bare.
func summarizeProject(r *http.Request, deps *Dependencies, project string) projectSummary {
	s := projectSummary{Name: project}

	memoryDoc, err := deps.DuffelClient.ReadFile(r.Context(), memory.FilePath(project))
	if err == nil {
		s.MemoryBytes = len(memoryDoc)
		s.MemoryTokens = memory.EstimateTokens(memoryDoc)
		s.HasMemory = strings.TrimSpace(memoryDoc) != ""
	}

	lessonsDoc, err := deps.DuffelClient.ReadFile(r.Context(), memory.LessonsFilePath(project))
	if err == nil {
		entries := memory.ParseJournalEntries(lessonsDoc)
		s.Lessons = len(entries)
		if len(entries) > 0 {
			s.LastActivity = entries[len(entries)-1].Timestamp
		}
	}
	return s
}

// getMemoryProjectHandler handles GET /memory/projects/{project}: returns the
// curated memory document, the raw lessons journal, and its parsed entries.
func getMemoryProjectHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireDuffel(deps, w) {
			return
		}
		project, ok := sanitizedProjectParam(w, r)
		if !ok {
			return
		}

		memoryDoc, err := deps.DuffelClient.ReadFile(r.Context(), memory.FilePath(project))
		if err != nil && !errors.Is(err, duffel.ErrNotFound) {
			writeError(w, http.StatusBadGateway, "duffel_error", err.Error())
			return
		}
		lessonsDoc, err := deps.DuffelClient.ReadFile(r.Context(), memory.LessonsFilePath(project))
		if err != nil && !errors.Is(err, duffel.ErrNotFound) {
			writeError(w, http.StatusBadGateway, "duffel_error", err.Error())
			return
		}

		type entry struct {
			Timestamp string `json:"timestamp"`
			Content   string `json:"content"`
		}
		parsed := memory.ParseJournalEntries(lessonsDoc)
		entries := make([]entry, 0, len(parsed))
		for _, e := range parsed {
			entries = append(entries, entry{Timestamp: e.Timestamp, Content: e.Content})
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"project": project,
			"memory":  memoryDoc,
			"lessons": entries,
		})
	}
}

// putMemoryDocHandler handles PUT /memory/projects/{project}/memory: replaces
// the curated memory document (human curation always wins).
func putMemoryDocHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireDuffel(deps, w) {
			return
		}
		project, ok := sanitizedProjectParam(w, r)
		if !ok {
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if strings.TrimSpace(body.Content) == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "content is required")
			return
		}
		if err := deps.DuffelClient.WriteFile(r.Context(), memory.FilePath(project), body.Content); err != nil {
			writeError(w, http.StatusBadGateway, "duffel_error", err.Error())
			return
		}
		deps.Logger.Info().Str("project", project).Msg("memory.md edited via admin")
		writeJSON(w, http.StatusOK, map[string]string{"status": "saved", "project": project})
	}
}

// appendLessonHandler handles POST /memory/projects/{project}/lessons:
// appends a curator-authored entry to the lessons journal.
func appendLessonHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireDuffel(deps, w) {
			return
		}
		project, ok := sanitizedProjectParam(w, r)
		if !ok {
			return
		}
		var body struct {
			Content string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if strings.TrimSpace(body.Content) == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "content is required")
			return
		}
		entry := strings.TrimSpace(body.Content) + "\n\n(source: admin)"
		unlock := memory.LockProject(project)
		defer unlock()
		if err := deps.DuffelClient.JournalAppend(r.Context(), memory.LessonsFilePath(project), entry); err != nil {
			writeError(w, http.StatusBadGateway, "duffel_error", err.Error())
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"status": "recorded", "project": project})
	}
}

// archiveLessonsHandler handles POST /memory/projects/{project}/lessons/archive:
// archives the journal (recoverable under .archive/) and starts a fresh one.
func archiveLessonsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireDuffel(deps, w) {
			return
		}
		project, ok := sanitizedProjectParam(w, r)
		if !ok {
			return
		}
		unlock := memory.LockProject(project)
		defer unlock()
		if err := deps.DuffelClient.Archive(r.Context(), memory.LessonsFilePath(project)); err != nil {
			if errors.Is(err, duffel.ErrNotFound) {
				writeError(w, http.StatusNotFound, "not_found", "no lessons journal for this project")
				return
			}
			writeError(w, http.StatusBadGateway, "duffel_error", err.Error())
			return
		}
		marker := "Journal archived via admin (previous entries recoverable under .archive/)."
		if err := deps.DuffelClient.JournalAppend(r.Context(), memory.LessonsFilePath(project), marker); err != nil {
			writeError(w, http.StatusBadGateway, "duffel_error", err.Error())
			return
		}
		deps.Logger.Info().Str("project", project).Msg("lessons journal archived via admin")
		writeJSON(w, http.StatusOK, map[string]string{"status": "archived", "project": project})
	}
}

// bundlePreviewHandler handles GET /memory/projects/{project}/bundle: assembles
// the exact context bundle an agent receives for this project and returns it
// read-only, along with the token estimate and degraded status. This is the
// ground truth of "what the model actually sees" — the surest way to catch the
// silent degradation that happens when duffel is unreachable.
func bundlePreviewHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.MemoryBuilder == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "injected-bundle preview is not configured on the admin")
			return
		}
		project, ok := sanitizedProjectParam(w, r)
		if !ok {
			return
		}
		// An optional hint mirrors the per-session semantic search the proxy runs
		// when a session supplies one; empty hint previews the cached base bundle.
		hint := strings.TrimSpace(r.URL.Query().Get("hint"))

		bundle := deps.MemoryBuilder.Build(r.Context(), project, hint, 0, true)
		writeJSON(w, http.StatusOK, map[string]any{
			"project":         bundle.Project,
			"markdown":        bundle.Markdown,
			"token_estimate":  bundle.TokenEstimate,
			"degraded":        bundle.Degraded,
			"degraded_reason": bundle.DegradedReason,
			"from_cache":      bundle.FromCache,
			"hint":            hint,
		})
	}
}

// searchMemoryHandler handles GET /memory/search?q=...&limit=...: runs the
// embedded duffel semantic search across all project memory so operators can
// find a lesson or fact without knowing which project recorded it.
func searchMemoryHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !requireDuffel(deps, w) {
			return
		}
		query := strings.TrimSpace(r.URL.Query().Get("q"))
		if query == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "q is required")
			return
		}
		limit := 10
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 50 {
				limit = n
			}
		}
		results, err := deps.DuffelClient.Search(r.Context(), query, duffel.SearchOpts{Limit: limit})
		if err != nil {
			writeError(w, http.StatusBadGateway, "duffel_error", err.Error())
			return
		}
		if results == nil {
			results = []duffel.SearchResult{}
		}
		writeJSON(w, http.StatusOK, map[string]any{"query": query, "results": results})
	}
}
