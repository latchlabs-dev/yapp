package insights

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/duffel"
	insightsv1 "github.com/earlgrey/piproxy/internal/insightsrpc/v1"
	"github.com/earlgrey/piproxy/internal/logging"
	"github.com/earlgrey/piproxy/internal/memory"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type sessionMarker interface {
	MarkDistilledFor(context.Context, pgstore.SessionIdentity, string) error
}

type sessionLogSource interface {
	GetBySession(context.Context, string, string, string) (*pgstore.PiSessionLogRecord, []byte, error)
}

type insightStore interface {
	ListMatchingConfigurations(context.Context, string, string) ([]*pgstore.InsightConfigurationRecord, error)
	ListTags(context.Context) ([]*pgstore.InsightTagRecord, error)
	CreateRun(context.Context, *pgstore.InsightRunRecord) (bool, error)
	CompleteRun(context.Context, string, string, []*pgstore.InsightRecord, []*pgstore.InsightTagRecord, json.RawMessage, json.RawMessage) error
	FailRun(context.Context, string, string) error
	GetRunResults(context.Context, string) (*pgstore.InsightRunRecord, []*pgstore.InsightRecord, error)
}

type journal interface {
	JournalAppend(context.Context, string, string) error
	ReadFile(context.Context, string) (string, error)
}

type generatorClient interface {
	Generate(context.Context, *pgstore.InsightProcessorRecord, *insightsv1.GenerateInsightsRequest) (*insightsv1.GenerateInsightsResponse, error)
}

// Coordinator is PiProxy's control-plane side of the service boundary.
// It parses canonical sessions, resolves configurations and tags, persists
// provenance, and materializes accepted results into Duffel.
type Coordinator struct {
	sessions sessionMarker
	logs     sessionLogSource
	store    insightStore
	clients  generatorClient
	duffel   journal
	logger   zerolog.Logger
}

func NewCoordinator(sessions sessionMarker, logs sessionLogSource, store insightStore, clients generatorClient, journal journal, logger zerolog.Logger) *Coordinator {
	return &Coordinator{sessions: sessions, logs: logs, store: store, clients: clients, duffel: journal, logger: logger}
}

func (c *Coordinator) AllowsProject(ctx context.Context, tenantID, projectID string) (bool, error) {
	if c == nil || c.store == nil || strings.TrimSpace(tenantID) == "" || strings.TrimSpace(projectID) == "" {
		return false, nil
	}
	configs, err := c.store.ListMatchingConfigurations(ctx, tenantID, projectID)
	return len(configs) > 0, err
}

func (c *Coordinator) DistillOne(ctx context.Context, rec *pgstore.SessionSubmissionRecord) error {
	identity := rec.Identity()
	logRecord, raw, err := c.logs.GetBySession(ctx, rec.TenantID, rec.OwnerID, rec.ConversationID)
	if err != nil {
		if errors.Is(err, pgstore.ErrSessionLogNotFound) {
			return c.sessions.MarkDistilledFor(ctx, identity, "skipped: no canonical session log")
		}
		return fmt.Errorf("get canonical session log: %w", err)
	}
	projectID := strings.TrimSpace(rec.ProjectID)
	if projectID == "" {
		projectID = strings.TrimSpace(logRecord.ProjectID)
	}
	if projectID == "" {
		return c.sessions.MarkDistilledFor(ctx, identity, "skipped: unresolved repository identity")
	}
	projectID, err = memory.SanitizeProject(projectID)
	if err != nil {
		return c.sessions.MarkDistilledFor(ctx, identity, "skipped: invalid project: "+err.Error())
	}
	storageProject, err := memory.ScopedProject(rec.TenantID, projectID)
	if err != nil {
		return c.sessions.MarkDistilledFor(ctx, identity, "skipped: invalid tenant project scope")
	}
	configs, err := c.store.ListMatchingConfigurations(ctx, rec.TenantID, projectID)
	if err != nil {
		return fmt.Errorf("resolve insight configurations: %w", err)
	}
	if len(configs) == 0 {
		return c.sessions.MarkDistilledFor(ctx, identity, "skipped: no active insight configurations")
	}
	tags, err := c.store.ListTags(ctx)
	if err != nil {
		return fmt.Errorf("load current insight tags: %w", err)
	}
	currentTags := make([]*insightsv1.TagDefinition, 0, len(tags))
	for _, tag := range tags {
		if tag.Status == "active" {
			currentTags = append(currentTags, &insightsv1.TagDefinition{Name: tag.Name, Description: tag.Description, UsageCount: tag.UsageCount})
		}
	}
	digest := sha256.Sum256(raw)
	inputSHA := hex.EncodeToString(digest[:])
	for _, config := range configs {
		transcript := ParseTranscript(raw, rec.ConversationID, projectID, config.MaxInputItems, config.MaxInputChars)
		if len(transcript.Items) == 0 {
			continue
		}
		run := &pgstore.InsightRunRecord{
			ConfigurationID: config.ID, ConfigurationRevision: config.Revision,
			SessionSubmissionID: rec.ID, TenantID: rec.TenantID, OwnerID: rec.OwnerID,
			ConversationID: rec.ConversationID, ProjectID: projectID, InputSHA256: inputSHA,
		}
		_, err := c.store.CreateRun(ctx, run)
		if err != nil {
			return err
		}
		if run.Status == "completed" {
			completedRun, results, err := c.store.GetRunResults(ctx, run.ID)
			if err != nil {
				return err
			}
			if err := c.materialize(ctx, storageProject, completedRun, results); err != nil {
				return err
			}
			continue
		}
		response, err := c.clients.Generate(ctx, config.Processor, &insightsv1.GenerateInsightsRequest{
			RunId: run.ID, ConfigurationId: config.ID, Transcript: transcript,
			CurrentTags: currentTags, Instructions: config.Instructions,
			AllowNewTags: config.AllowNewTags, IncludeSessionSummary: config.IncludeSessionSummary,
			IncludeInsightSummaries: config.IncludeInsightSummaries,
		})
		if err != nil {
			if failErr := c.store.FailRun(ctx, run.ID, err.Error()); failErr != nil {
				return errors.Join(err, failErr)
			}
			return err
		}
		if response == nil || (response.GetRunId() != "" && response.GetRunId() != run.ID) {
			err = fmt.Errorf("insight processor returned an invalid run identity")
			if failErr := c.store.FailRun(ctx, run.ID, err.Error()); failErr != nil {
				return errors.Join(err, failErr)
			}
			return err
		}
		results, proposed, usage, warnings := normalizeResponse(response, transcript, currentTags, config.AllowNewTags)
		if err := c.store.CompleteRun(ctx, run.ID, shortSummary(response.GetSessionSummary()), results, proposed, usage, warnings); err != nil {
			return err
		}
		completedRun, storedResults, err := c.store.GetRunResults(ctx, run.ID)
		if err != nil {
			return err
		}
		if err := c.materialize(ctx, storageProject, completedRun, storedResults); err != nil {
			return err
		}
		c.logger.Info().Str("run_id", run.ID).Str("configuration_id", config.ID).
			Str("conversation_id", rec.ConversationID).Int("insights", len(storedResults)).Msg("session insights generated")
	}
	return c.sessions.MarkDistilledFor(ctx, identity, "")
}

func normalizeResponse(response *insightsv1.GenerateInsightsResponse, transcript *insightsv1.ParsedTranscript, currentTags []*insightsv1.TagDefinition, allowNewTags bool) ([]*pgstore.InsightRecord, []*pgstore.InsightTagRecord, json.RawMessage, json.RawMessage) {
	knownItems := make(map[string]struct{}, len(transcript.Items))
	for _, item := range transcript.Items {
		knownItems[item.Id] = struct{}{}
	}
	knownTags := make(map[string]struct{}, len(currentTags))
	for _, tag := range currentTags {
		knownTags[normalizeTag(tag.GetName())] = struct{}{}
	}
	results := make([]*pgstore.InsightRecord, 0, min(len(response.GetInsights()), 100))
	for _, candidate := range response.GetInsights() {
		if len(results) == 100 {
			break
		}
		content := strings.TrimSpace(logging.Redact(candidate.GetContent()))
		if content == "" {
			continue
		}
		if runes := []rune(content); len(runes) > 65536 {
			content = string(runes[:65536])
		}
		tags := make([]string, 0, len(candidate.GetTags()))
		for _, tag := range candidate.GetTags() {
			normalized := normalizeTag(tag)
			_, known := knownTags[normalized]
			if normalized != "" && (allowNewTags || known) && !slices.Contains(tags, normalized) {
				tags = append(tags, normalized)
			}
		}
		sources := make([]string, 0, len(candidate.GetSourceItemIds()))
		for _, source := range candidate.GetSourceItemIds() {
			if _, ok := knownItems[source]; ok && !slices.Contains(sources, source) {
				sources = append(sources, source)
			}
		}
		metadata := json.RawMessage(`{}`)
		if candidate.Metadata != nil {
			if encoded, err := json.Marshal(candidate.Metadata.AsMap()); err == nil {
				metadata = encoded
			}
		}
		results = append(results, &pgstore.InsightRecord{Summary: shortSummary(logging.Redact(candidate.GetSummary())), Content: content, Tags: tags, SourceItemIDs: sources, Metadata: metadata})
	}
	proposed := make([]*pgstore.InsightTagRecord, 0, len(response.GetProposedTags()))
	for _, tag := range response.GetProposedTags() {
		if !allowNewTags {
			break
		}
		if name := normalizeTag(tag.GetName()); name != "" {
			proposed = append(proposed, &pgstore.InsightTagRecord{Name: name, Description: strings.TrimSpace(tag.GetDescription()), Status: "active"})
		}
	}
	usage, _ := json.Marshal(response.GetUsage())
	warnings, _ := json.Marshal(response.GetWarnings())
	return results, proposed, usage, warnings
}

func (c *Coordinator) materialize(ctx context.Context, project string, run *pgstore.InsightRunRecord, results []*pgstore.InsightRecord) error {
	if run.SessionSummary == "" && len(results) == 0 {
		return nil
	}
	marker := "(insight run: " + run.ID + ", configuration: " + run.ConfigurationID + ")"
	path := memory.LessonsFilePath(project)
	unlock := memory.LockProject(project)
	defer unlock()
	existing, err := c.duffel.ReadFile(ctx, path)
	if err == nil && strings.Contains(existing, marker) {
		return nil
	}
	if err != nil && !errors.Is(err, duffel.ErrNotFound) {
		return fmt.Errorf("read insight journal for idempotency: %w", err)
	}
	var entry strings.Builder
	entry.WriteString("(source: distiller, conversation: " + run.ConversationID + ")\n\n")
	entry.WriteString(marker + "\n")
	if run.SessionSummary != "" {
		entry.WriteString("\n**Session summary:** " + run.SessionSummary + "\n")
	}
	for _, result := range results {
		label := strings.Join(result.Tags, ", ")
		if label == "" {
			label = "insight"
		}
		entry.WriteString("\n**[" + label + "]**")
		if result.Summary != "" {
			entry.WriteString(" " + result.Summary)
		}
		entry.WriteString("\n\n" + result.Content + "\n")
	}
	if err := c.duffel.JournalAppend(ctx, path, entry.String()); err != nil {
		return fmt.Errorf("append insight journal: %w", err)
	}
	return nil
}

func normalizeTag(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), "-")
}

func shortSummary(value string) string {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(value), "\r\n", "\n"), "\n")
	out := make([]string, 0, 2)
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
			if len(out) == 2 {
				break
			}
		}
	}
	return strings.Join(out, "\n")
}
