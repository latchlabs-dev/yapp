package admin

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/memory"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// ProjectStore is the repository-identity read model used by the admin API.
// The concrete Postgres store keeps deduplication on normalized Git remotes.
type ProjectStore interface {
	ListAdmin(ctx context.Context) ([]*pgstore.AdminMemoryProject, error)
	GetAdmin(ctx context.Context, projectID string) (*pgstore.AdminMemoryProject, error)
	AttributionCoverage(ctx context.Context) (*pgstore.ProjectAttributionCoverage, error)
}

// ProjectUserStore resolves a server-recorded user ID inside the project's
// tenant. It is deliberately tenant-scoped so journal content cannot resolve a
// user from another tenant.
type ProjectUserStore interface {
	GetByID(ctx context.Context, tenantID, userID string) (*pgstore.UserRecord, error)
}

type projectKnowledgeEntry struct {
	Timestamp   string                       `json:"timestamp"`
	Content     string                       `json:"content"`
	Attribution *projectKnowledgeAttribution `json:"attribution,omitempty"`
}

type projectKnowledgeAttribution struct {
	Source         string                    `json:"source"`
	ConversationID string                    `json:"conversation_id,omitempty"`
	User           *pgstore.AdminProjectUser `json:"user,omitempty"`
	UserID         string                    `json:"-"`
}

var projectAgentSourcePattern = regexp.MustCompile(`\(source:\s*agent(?:,\s*user:\s*([^)]+))?\)$`)

const projectDistillerSourcePrefix = "(source: distiller, conversation: "

type projectKnowledgeSummary struct {
	Available             bool   `json:"available"`
	HasMemory             bool   `json:"has_memory"`
	MemoryTokens          int    `json:"memory_tokens"`
	MemoryBytes           int    `json:"memory_bytes"`
	InsightCount          int    `json:"insight_count"`
	PromptReflectionCount int    `json:"prompt_reflection_count"`
	LastKnowledgeActivity string `json:"last_knowledge_activity,omitempty"`
}

type repositoryProjectSummary struct {
	*pgstore.AdminMemoryProject
	Participants []projectParticipantSummary `json:"participants"`
	Knowledge    projectKnowledgeSummary     `json:"knowledge"`
}

type projectParticipantSummary struct {
	Name string `json:"name"`
}

type repositoryProjectDetail struct {
	*pgstore.AdminMemoryProject
	Knowledge         projectKnowledgeSummary `json:"knowledge"`
	Memory            string                  `json:"memory"`
	Insights          []projectKnowledgeEntry `json:"insights"`
	PromptReflections []projectKnowledgeEntry `json:"prompt_reflections"`
}

func listProjectsHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.MemoryProjectStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "project store not configured")
			return
		}
		projects, err := deps.MemoryProjectStore.ListAdmin(r.Context())
		if err != nil {
			deps.Logger.Error().Err(err).Msg("repository projects list failed")
			writeError(w, http.StatusInternalServerError, "list_projects", err.Error())
			return
		}
		coverage, err := deps.MemoryProjectStore.AttributionCoverage(r.Context())
		if err != nil {
			deps.Logger.Error().Err(err).Msg("project attribution coverage failed")
			writeError(w, http.StatusInternalServerError, "project_attribution_coverage", err.Error())
			return
		}

		result := make([]repositoryProjectSummary, len(projects))
		var wg sync.WaitGroup
		reads := make(chan struct{}, 8)
		for index, project := range projects {
			wg.Go(func() {
				reads <- struct{}{}
				defer func() { <-reads }()
				detail := readRepositoryProjectKnowledge(r.Context(), deps.DuffelClient, nil, project)
				result[index] = repositoryProjectSummary{
					AdminMemoryProject: project,
					Participants:       summarizeProjectParticipants(project.Participants),
					Knowledge:          detail.Knowledge,
				}
			})
		}
		wg.Wait()
		writeJSON(w, http.StatusOK, map[string]any{
			"projects":             result,
			"attribution_coverage": coverage,
		})
	}
}

func getProjectHandler(deps *Dependencies) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.MemoryProjectStore == nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", "project store not configured")
			return
		}
		projectID := strings.TrimSpace(chi.URLParam(r, "projectID"))
		if projectID == "" {
			writeError(w, http.StatusBadRequest, "validation_error", "project_id is required")
			return
		}
		project, err := deps.MemoryProjectStore.GetAdmin(r.Context(), projectID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "not_found", "project not found")
				return
			}
			deps.Logger.Error().Err(err).Str("project_id", projectID).Msg("repository project read failed")
			writeError(w, http.StatusInternalServerError, "get_project", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, readRepositoryProjectKnowledge(r.Context(), deps.DuffelClient, deps.ProjectUserStore, project))
	}
}

func readRepositoryProjectKnowledge(ctx context.Context, client *duffel.Client, users ProjectUserStore, project *pgstore.AdminMemoryProject) repositoryProjectDetail {
	detail := repositoryProjectDetail{
		AdminMemoryProject: project,
		Insights:           []projectKnowledgeEntry{},
		PromptReflections:  []projectKnowledgeEntry{},
	}
	if client == nil {
		return detail
	}

	storageProject, err := memory.ScopedProject(project.TenantID, project.ID)
	if err != nil {
		return detail
	}
	detail.Knowledge.Available = true

	memoryDoc, err := client.ReadFile(ctx, memory.FilePath(storageProject))
	if err == nil {
		detail.Memory = memoryDoc
		detail.Knowledge.MemoryBytes = len(memoryDoc)
		detail.Knowledge.MemoryTokens = memory.EstimateTokens(memoryDoc)
		detail.Knowledge.HasMemory = hasCuratedProjectMemory(memoryDoc)
	} else if !errors.Is(err, duffel.ErrNotFound) {
		detail.Knowledge.Available = false
	}

	lessonsDoc, err := client.ReadFile(ctx, memory.LessonsFilePath(storageProject))
	if err == nil {
		detail.Insights = meaningfulKnowledgeEntries(memory.ParseJournalEntries(lessonsDoc), project)
		detail.Knowledge.InsightCount = len(detail.Insights)
		updateLastKnowledgeActivity(&detail.Knowledge, detail.Insights)
	} else if !errors.Is(err, duffel.ErrNotFound) {
		detail.Knowledge.Available = false
	}

	reflectionsDoc, err := client.ReadFile(ctx, memory.PromptReflectionsFilePath(storageProject))
	if err == nil {
		detail.PromptReflections = meaningfulKnowledgeEntries(memory.ParseJournalEntries(reflectionsDoc), project)
		detail.Knowledge.PromptReflectionCount = len(detail.PromptReflections)
		updateLastKnowledgeActivity(&detail.Knowledge, detail.PromptReflections)
	} else if !errors.Is(err, duffel.ErrNotFound) {
		detail.Knowledge.Available = false
	}
	resolveKnowledgeEntryUsers(ctx, users, project.TenantID, detail.Insights, detail.PromptReflections)

	return detail
}

func summarizeProjectParticipants(participants []pgstore.AdminProjectParticipant) []projectParticipantSummary {
	result := make([]projectParticipantSummary, 0, len(participants))
	for _, participant := range participants {
		name := strings.TrimSpace(participant.DisplayName)
		if name == "" {
			name = "Unnamed participant"
		}
		result = append(result, projectParticipantSummary{Name: name})
	}
	return result
}

func meaningfulKnowledgeEntries(entries []memory.JournalEntry, project *pgstore.AdminMemoryProject) []projectKnowledgeEntry {
	result := make([]projectKnowledgeEntry, 0, len(entries))
	for _, entry := range entries {
		content := strings.TrimSpace(entry.Content)
		if content == "" || strings.HasPrefix(content, "Project memory initialized") ||
			strings.HasPrefix(content, "Journal archived via admin") ||
			strings.Contains(content, "Compacted into memory.md (previous journal archived under .archive/).") {
			continue
		}
		result = append(result, projectKnowledgeEntry{
			Timestamp:   entry.Timestamp,
			Content:     content,
			Attribution: projectKnowledgeAttributionFor(content, project),
		})
	}
	return result
}

func projectKnowledgeAttributionFor(content string, project *pgstore.AdminMemoryProject) *projectKnowledgeAttribution {
	firstLine, _, _ := strings.Cut(strings.TrimSpace(content), "\n")
	firstLine = strings.TrimSpace(firstLine)
	if match := projectAgentSourcePattern.FindStringSubmatch(firstLine); match != nil {
		attribution := &projectKnowledgeAttribution{Source: "agent"}
		if userID := strings.TrimSpace(match[1]); userID != "" {
			attribution.UserID = userID
			attribution.User = project.InsightUser("user", userID)
		}
		return attribution
	}
	if !strings.HasPrefix(firstLine, projectDistillerSourcePrefix) || !strings.HasSuffix(firstLine, ")") {
		return nil
	}
	attribution := &projectKnowledgeAttribution{Source: "distiller"}
	if conversationID, user := project.MatchDistillerInsight(strings.TrimSpace(content)); conversationID != "" {
		attribution.ConversationID = conversationID
		attribution.User = user
		return attribution
	}
	// Unknown or legacy sessions can still show their first-line identifier, but
	// never use this syntactic fallback to resolve a user.
	attribution.ConversationID = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(firstLine, projectDistillerSourcePrefix), ")"))
	return attribution
}

func resolveKnowledgeEntryUsers(ctx context.Context, users ProjectUserStore, tenantID string, groups ...[]projectKnowledgeEntry) {
	if users == nil || tenantID == "" {
		return
	}
	resolved := make(map[string]*pgstore.AdminProjectUser)
	for _, entries := range groups {
		for index := range entries {
			attribution := entries[index].Attribution
			if attribution == nil || attribution.User != nil || attribution.UserID == "" {
				continue
			}
			user, seen := resolved[attribution.UserID]
			if !seen {
				record, err := users.GetByID(ctx, tenantID, attribution.UserID)
				if err == nil {
					user = &pgstore.AdminProjectUser{ID: record.ID, DisplayName: record.DisplayName, Email: record.Email}
				}
				resolved[attribution.UserID] = user
			}
			attribution.User = user
		}
	}
}

func hasCuratedProjectMemory(document string) bool {
	for line := range strings.SplitSeq(document, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "# Project memory:") ||
			strings.HasPrefix(line, "Repository:") ||
			strings.HasPrefix(line, "_Curated knowledge for this project. Maintained automatically:") {
			continue
		}
		return true
	}
	return false
}

func updateLastKnowledgeActivity(summary *projectKnowledgeSummary, entries []projectKnowledgeEntry) {
	for _, entry := range entries {
		// Duffel timestamps are fixed-width YYYY-MM-DD HH:MM strings, so lexical
		// comparison preserves chronological order.
		if entry.Timestamp > summary.LastKnowledgeActivity {
			summary.LastKnowledgeActivity = entry.Timestamp
		}
	}
}
