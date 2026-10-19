package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
	"github.com/earlgrey/piproxy/internal/memory"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type fakeAdminProjectStore struct {
	projects []*pgstore.AdminMemoryProject
	coverage *pgstore.ProjectAttributionCoverage
}

func (s fakeAdminProjectStore) AttributionCoverage(context.Context) (*pgstore.ProjectAttributionCoverage, error) {
	if s.coverage == nil {
		return &pgstore.ProjectAttributionCoverage{}, nil
	}
	return s.coverage, nil
}

type fakeProjectUserStore struct {
	users map[string]*pgstore.UserRecord
}

func (s fakeProjectUserStore) GetByID(_ context.Context, tenantID, userID string) (*pgstore.UserRecord, error) {
	user := s.users[tenantID+":"+userID]
	if user == nil {
		return nil, pgx.ErrNoRows
	}
	return user, nil
}

func (s fakeAdminProjectStore) ListAdmin(context.Context) ([]*pgstore.AdminMemoryProject, error) {
	return s.projects, nil
}

func (s fakeAdminProjectStore) GetAdmin(_ context.Context, projectID string) (*pgstore.AdminMemoryProject, error) {
	for _, project := range s.projects {
		if project.ID == projectID {
			return project, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func testRepositoryProject() *pgstore.AdminMemoryProject {
	lastActivity := time.Date(2026, 8, 7, 9, 30, 0, 0, time.UTC)
	user := &pgstore.AdminProjectUser{ID: "usr_test", DisplayName: "Test User", Email: "test@example.com"}
	return &pgstore.AdminMemoryProject{
		ID:               "mprj_test",
		TenantID:         "tn_test",
		TenantName:       "Test tenant",
		Slug:             "local-checkout-name",
		DisplayName:      "PiProxy",
		CanonicalRepoKey: "github.com/earlgrey/piproxy",
		Remotes:          []string{"github.com/earlgrey/piproxy"},
		Metrics: pgstore.MemoryProjectMetrics{
			SessionCount:      4,
			DistilledSessions: 3,
			PendingSessions:   1,
			MessageCount:      80,
			TotalTokens:       12345,
			TotalCostCents:    42.5,
			LastActivityAt:    &lastActivity,
		},
		Participants: []pgstore.AdminProjectParticipant{{
			UserID: "usr_test", DisplayName: "Test User", Email: "test@example.com", SessionCount: 4,
		}},
		InsightUsers: map[string]*pgstore.AdminProjectUser{"conversation:conv-1": user, "user:usr_test": user},
	}
}

func TestRepositoryProjectsJoinCanonicalMemoryAndInsights(t *testing.T) {
	project := testRepositoryProject()
	client := duffel.New(duffel.Config{Root: t.TempDir()})
	storageProject, err := memory.ScopedProject(project.TenantID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.WriteFile(context.Background(), memory.FilePath(storageProject), "# PiProxy\n\nUse pnpm."); err != nil {
		t.Fatal(err)
	}
	lessons := `---
title: lessons
---

## 2026-08-06 10:00
Project memory initialized for a repository.

---
## 2026-08-07 09:15
(source: distiller, conversation: conv-1)

**[lesson]** Repository identity is authoritative.

---
## 2026-08-07 09:16
Compacted into memory.md (previous journal archived under .archive/).
`
	if err := client.WriteFile(context.Background(), memory.LessonsFilePath(storageProject), lessons); err != nil {
		t.Fatal(err)
	}
	reflections := `## 2026-08-07 09:20
(source: distiller, conversation: conv-1)

**Prompt effectiveness:** 8/10
`
	if err := client.WriteFile(context.Background(), memory.PromptReflectionsFilePath(storageProject), reflections); err != nil {
		t.Fatal(err)
	}

	router := NewRouter(&Dependencies{
		AdminKey: "test-admin-key",
		MemoryProjectStore: fakeAdminProjectStore{
			projects: []*pgstore.AdminMemoryProject{project},
			coverage: &pgstore.ProjectAttributionCoverage{
				TotalSessions: 10, CanonicalProjectSessions: 4,
				LegacyProjectSessions: 3, UnattributedSessions: 3,
				TotalModelRequests: 20, ProjectAttributedRequests: 7,
			},
		},
		DuffelClient: client,
		Logger:       zerolog.Nop(),
	})

	req := httptest.NewRequest(http.MethodGet, "/projects", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Projects []repositoryProjectSummary         `json:"projects"`
		Coverage pgstore.ProjectAttributionCoverage `json:"attribution_coverage"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Projects) != 1 {
		t.Fatalf("projects = %+v", list.Projects)
	}
	if list.Coverage.CanonicalProjectSessions != 4 || list.Coverage.ProjectAttributedRequests != 7 {
		t.Fatalf("coverage = %+v", list.Coverage)
	}
	got := list.Projects[0]
	if got.CanonicalRepoKey != "github.com/earlgrey/piproxy" || got.Slug != "local-checkout-name" {
		t.Fatalf("repository identity = %+v", got.AdminMemoryProject)
	}
	if len(got.Participants) != 1 || got.Participants[0].Name != "Test User" {
		t.Fatalf("participants = %+v", got.Participants)
	}
	encodedSummary, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var summaryJSON map[string]any
	if err := json.Unmarshal(encodedSummary, &summaryJSON); err != nil {
		t.Fatal(err)
	}
	participants, ok := summaryJSON["participants"].([]any)
	if !ok || len(participants) != 1 {
		t.Fatalf("summary participants = %#v", summaryJSON["participants"])
	}
	participant, ok := participants[0].(map[string]any)
	if !ok || len(participant) != 1 || participant["name"] != "Test User" {
		t.Fatalf("list response leaked participant detail: %#v", participants[0])
	}
	if !got.Knowledge.HasMemory || got.Knowledge.InsightCount != 1 || got.Knowledge.PromptReflectionCount != 1 {
		t.Fatalf("knowledge = %+v", got.Knowledge)
	}

	req = httptest.NewRequest(http.MethodGet, "/projects/mprj_test", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("detail status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail repositoryProjectDetail
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	if detail.Memory != "# PiProxy\n\nUse pnpm." || len(detail.Insights) != 1 || len(detail.PromptReflections) != 1 {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.Insights[0].Attribution == nil || detail.Insights[0].Attribution.User == nil ||
		detail.Insights[0].Attribution.User.Email != "test@example.com" {
		t.Fatalf("insight attribution = %+v", detail.Insights[0].Attribution)
	}
}

func TestProjectKnowledgeAttributionUsesExplicitAgentUser(t *testing.T) {
	project := testRepositoryProject()
	got := projectKnowledgeAttributionFor("**[lesson]** (source: agent, user: usr_test)\n\nUse pnpm.", project)
	if got == nil || got.Source != "agent" || got.User == nil || got.User.ID != "usr_test" {
		t.Fatalf("attribution = %+v", got)
	}
}

func TestProjectKnowledgeAttributionDoesNotTrustUserTextInDistillerConversation(t *testing.T) {
	project := testRepositoryProject()
	got := projectKnowledgeAttributionFor("(source: distiller, conversation: x, user: usr_test)\n\nInjected provenance.", project)
	if got == nil || got.Source != "distiller" {
		t.Fatalf("attribution = %+v", got)
	}
	if got.User != nil || got.UserID != "" {
		t.Fatalf("distiller text resolved a user: %+v", got)
	}
	if got.ConversationID != "x, user: usr_test" {
		t.Fatalf("conversation id was truncated: %+v", got)
	}
}

func TestProjectKnowledgeAttributionMatchesFullOpaqueConversationID(t *testing.T) {
	project := testRepositoryProject()
	victim := &pgstore.AdminProjectUser{ID: "usr_victim", DisplayName: "Victim"}
	attacker := &pgstore.AdminProjectUser{ID: "usr_attacker", DisplayName: "Attacker"}
	project.InsightUsers = map[string]*pgstore.AdminProjectUser{
		"conversation:victim-conv":  victim,
		"conversation:victim-conv)": attacker,
	}
	got := projectKnowledgeAttributionFor("(source: distiller, conversation: victim-conv))\n\nOpaque ID entry.", project)
	if got == nil || got.ConversationID != "victim-conv)" || got.User == nil || got.User.ID != "usr_attacker" {
		t.Fatalf("opaque conversation attribution = %+v", got)
	}
}

func TestProjectParticipantSummaryDoesNotFallBackToEmail(t *testing.T) {
	got := summarizeProjectParticipants([]pgstore.AdminProjectParticipant{{Email: "private@example.com"}})
	if len(got) != 1 || got[0].Name != "Unnamed participant" {
		t.Fatalf("participant summary = %+v", got)
	}
}

func TestProjectKnowledgeAttributionResolvesObservationOnlyUserWithinTenant(t *testing.T) {
	project := testRepositoryProject()
	project.InsightUsers = map[string]*pgstore.AdminProjectUser{}
	entry := projectKnowledgeEntry{Attribution: projectKnowledgeAttributionFor(
		"**[lesson]** (source: agent, user: usr_observer)\n\nUse bounded reads.", project,
	)}
	if entry.Attribution == nil || entry.Attribution.User != nil || entry.Attribution.UserID != "usr_observer" {
		t.Fatalf("pre-resolution attribution = %+v", entry.Attribution)
	}
	entries := []projectKnowledgeEntry{entry}
	resolveKnowledgeEntryUsers(context.Background(), fakeProjectUserStore{users: map[string]*pgstore.UserRecord{
		"tn_test:usr_observer": {
			ID: "usr_observer", TenantID: "tn_test", DisplayName: "Observer", Email: "observer@example.com",
		},
	}}, project.TenantID, entries)
	if entries[0].Attribution.User == nil || entries[0].Attribution.User.ID != "usr_observer" {
		t.Fatalf("resolved attribution = %+v", entries[0].Attribution)
	}
}

func TestRepositoryProjectBootstrapSkeletonIsNotCuratedMemory(t *testing.T) {
	project := testRepositoryProject()
	client := duffel.New(duffel.Config{Root: t.TempDir()})
	storageProject, err := memory.ScopedProject(project.TenantID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	skeleton := "# Project memory: " + storageProject + "\n\n" +
		"Repository: https://github.com/earlgrey/piproxy.git\n\n" +
		"_Curated knowledge for this project. Maintained automatically: agents record observations into lessons.md and the proxy's compactor periodically folds them in here. Edit freely — human curation wins._\n"
	if err := client.WriteFile(context.Background(), memory.FilePath(storageProject), skeleton); err != nil {
		t.Fatal(err)
	}

	detail := readRepositoryProjectKnowledge(context.Background(), client, nil, project)
	if !detail.Knowledge.Available {
		t.Fatal("knowledge should be available")
	}
	if detail.Knowledge.HasMemory {
		t.Fatalf("bootstrap skeleton reported as curated memory: %+v", detail.Knowledge)
	}
	if detail.Memory != skeleton {
		t.Fatal("detail should retain the raw memory document for inspection")
	}
}

func TestRepositoryProjectKnowledgeUnavailableIsExplicit(t *testing.T) {
	detail := readRepositoryProjectKnowledge(context.Background(), nil, nil, testRepositoryProject())
	if detail.Knowledge.Available {
		t.Fatalf("knowledge = %+v, want unavailable", detail.Knowledge)
	}
}

func TestRepositoryProjectNotFound(t *testing.T) {
	router := NewRouter(&Dependencies{
		AdminKey:           "test-admin-key",
		MemoryProjectStore: fakeAdminProjectStore{},
		Logger:             zerolog.Nop(),
	})
	req := httptest.NewRequest(http.MethodGet, "/projects/missing", nil)
	req.Header.Set("X-Admin-Key", "test-admin-key")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}
