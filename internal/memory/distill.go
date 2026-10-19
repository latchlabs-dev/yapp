package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/llmclient"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// DistillerConfig tunes one server-side session distillation task.
type DistillerConfig struct {
	Model          string // LLM used for distillation (required)
	MaxInputTokens int    // transcript budget for the distiller prompt (default 24000)
}

func (c DistillerConfig) withDefaults() DistillerConfig {
	if c.MaxInputTokens <= 0 {
		c.MaxInputTokens = 24000
	}
	return c
}

// Small interfaces so tests can inject fakes.

type distillationMarker interface {
	MarkDistilledFor(ctx context.Context, identity pgstore.SessionIdentity, distillErr string) error
}

// sessionLogSource yields the canonical Pi session JSONL for one owner-scoped
// session. session_submissions.conversation_id and pi_session_logs.session_id
// are the same Pi session id, so the queued conversation id joins directly.
type sessionLogSource interface {
	GetBySession(ctx context.Context, tenantID, ownerID, sessionID string) (*pgstore.PiSessionLogRecord, []byte, error)
}

type completer interface {
	Complete(ctx context.Context, req llmclient.Request) (string, error)
}

type journalAppender interface {
	JournalAppend(ctx context.Context, path, content string) error
}

type journalReader interface {
	ReadFile(ctx context.Context, path string) (string, error)
}

// Distiller extracts durable lessons for one queued session and appends them
// to the project's lessons journal in Duffel.
type Distiller struct {
	sessions distillationMarker
	logs     sessionLogSource
	llm      completer
	duffel   journalAppender
	cfg      DistillerConfig
	logger   zerolog.Logger
}

// NewDistiller creates a distiller.
func NewDistiller(sessions distillationMarker, logs sessionLogSource, llm completer, duffel journalAppender, cfg DistillerConfig, logger zerolog.Logger) *Distiller {
	return &Distiller{
		sessions: sessions,
		logs:     logs,
		llm:      llm,
		duffel:   duffel,
		cfg:      cfg.withDefaults(),
		logger:   logger,
	}
}

// Observation is a single distilled lesson/mistake/decision.
type Observation struct {
	Kind    string `json:"kind"`
	Content string `json:"content"`
}

// PromptImprovement describes one concrete way the user's prompting could
// have reduced rework or improved the resulting implementation.
type PromptImprovement struct {
	Issue           string `json:"issue"`
	BetterPrompt    string `json:"better_prompt"`
	ExpectedBenefit string `json:"expected_benefit"`
}

// PromptReflection is stored separately from durable project observations.
type PromptReflection struct {
	QualityScore int                 `json:"quality_score"`
	Strengths    []string            `json:"strengths"`
	Improvements []PromptImprovement `json:"improvements"`
}

// DistillationResult is the model's complete structured response.
type DistillationResult struct {
	Observations     []Observation    `json:"observations"`
	PromptReflection PromptReflection `json:"prompt_reflection"`
}

// DistillOne distills one Absurd-queued session from its canonical Pi session
// log (pi_session_logs). Sessions without a project or an uploaded canonical log
// are marked distilled with a skip note; transient failures return an error so
// Absurd can retry the task.
func (d *Distiller) DistillOne(ctx context.Context, rec *pgstore.SessionSubmissionRecord) error {
	identity := rec.Identity()

	logRec, jsonl, err := d.logs.GetBySession(ctx, rec.TenantID, rec.OwnerID, rec.ConversationID)
	if err != nil {
		if errors.Is(err, pgstore.ErrSessionLogNotFound) {
			return d.sessions.MarkDistilledFor(ctx, identity, "skipped: no canonical session log")
		}
		return fmt.Errorf("get canonical session log: %w", err)
	}

	projectID := rec.ProjectID
	if projectID == "" {
		projectID = logRec.ProjectID
	}
	if projectID == "" {
		return d.sessions.MarkDistilledFor(ctx, identity, "skipped: unresolved repository identity")
	}
	project, err := SanitizeProject(projectID)
	if err != nil {
		return d.sessions.MarkDistilledFor(ctx, identity, "skipped: invalid project: "+err.Error())
	}
	project, err = ScopedProject(rec.TenantID, project)
	if err != nil {
		return d.sessions.MarkDistilledFor(ctx, identity, "skipped: invalid tenant project scope")
	}

	// The journal append and Postgres completion marker cannot share a
	// transaction. If an append succeeds but a later write or MarkDistilled
	// fails, the row is eligible for retry. Detect the conversation marker in
	// both journals so retries append only the missing section and never create
	// duplicate entries.
	marker := "(source: distiller, conversation: " + rec.ConversationID + ")"
	lessonsPresent := false
	reflectionPresent := false
	if reader, ok := d.duffel.(journalReader); ok {
		lessonsPresent, err = journalContainsMarker(ctx, reader, LessonsFilePath(project), marker)
		if err != nil {
			return fmt.Errorf("read lessons journal for idempotency: %w", err)
		}
		reflectionPresent, err = journalContainsMarker(ctx, reader, PromptReflectionsFilePath(project), marker)
		if err != nil {
			return fmt.Errorf("read prompt reflections journal for idempotency: %w", err)
		}
		if lessonsPresent && reflectionPresent {
			return d.sessions.MarkDistilledFor(ctx, identity, "")
		}
	}

	transcript := FlattenPiSessionJSONL(jsonl, d.cfg.MaxInputTokens)
	if strings.TrimSpace(transcript) == "" {
		return d.sessions.MarkDistilledFor(ctx, identity, "skipped: empty transcript")
	}

	raw, err := d.llm.Complete(ctx, llmclient.Request{
		Model:     d.cfg.Model,
		System:    distillerSystemPrompt,
		User:      "Project: " + project + "\n\nSession transcript:\n\n" + transcript,
		MaxTokens: 1536,
	})
	if err != nil {
		return fmt.Errorf("distiller LLM call: %w", err)
	}

	result, err := ParseDistillationResult(raw)
	if err != nil {
		return fmt.Errorf("parse distiller output: %w", err)
	}
	if len(result.Observations) == 0 && result.PromptReflection.QualityScore == 0 {
		return d.sessions.MarkDistilledFor(ctx, identity, "no durable observations or prompt reflection")
	}

	unlock := LockProject(project)
	defer unlock()
	if len(result.Observations) > 0 && !lessonsPresent {
		entry := FormatDistilledEntry(result.Observations, rec.ConversationID)
		if err := d.duffel.JournalAppend(ctx, LessonsFilePath(project), entry); err != nil {
			return fmt.Errorf("append lessons journal: %w", err)
		}
	}
	if result.PromptReflection.QualityScore > 0 && !reflectionPresent {
		entry := FormatPromptReflectionEntry(result.PromptReflection, rec.ConversationID)
		if err := d.duffel.JournalAppend(ctx, PromptReflectionsFilePath(project), entry); err != nil {
			return fmt.Errorf("append prompt reflections journal: %w", err)
		}
	}

	d.logger.Info().
		Str("conversation_id", rec.ConversationID).
		Str("project", project).
		Int("observations", len(result.Observations)).
		Int("prompt_quality_score", result.PromptReflection.QualityScore).
		Msg("session distilled")
	return d.sessions.MarkDistilledFor(ctx, identity, "")
}

var validObservationKinds = map[string]bool{
	"mistake": true, "lesson": true, "decision": true, "context": true,
}

// ParseObservations parses the distiller's JSON output, tolerating markdown
// fences. Invalid kinds and empty contents are dropped, output is capped at 5.
func ParseDistillationResult(raw string) (DistillationResult, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	raw = strings.TrimSpace(raw)

	var parsed DistillationResult
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		extracted := extractJSONObject(raw)
		if extracted == raw {
			return DistillationResult{}, fmt.Errorf("invalid JSON: %w", err)
		}
		if err := json.Unmarshal([]byte(extracted), &parsed); err != nil {
			return DistillationResult{}, fmt.Errorf("invalid JSON: %w", err)
		}
	}

	out := make([]Observation, 0, min(len(parsed.Observations), 5))
	for _, obs := range parsed.Observations {
		obs.Kind = strings.ToLower(strings.TrimSpace(obs.Kind))
		obs.Content = strings.TrimSpace(obs.Content)
		if !validObservationKinds[obs.Kind] || obs.Content == "" {
			continue
		}
		out = append(out, obs)
		if len(out) == 5 {
			break
		}
	}
	parsed.Observations = out
	parsed.PromptReflection = normalizePromptReflection(parsed.PromptReflection)
	return parsed, nil
}

// ParseObservations preserves the focused parser API used by callers that
// only need the durable project-memory section.
func ParseObservations(raw string) ([]Observation, error) {
	result, err := ParseDistillationResult(raw)
	return result.Observations, err
}

func normalizePromptReflection(reflection PromptReflection) PromptReflection {
	if reflection.QualityScore < 1 || reflection.QualityScore > 10 {
		return PromptReflection{}
	}
	reflection.Strengths = cleanStrings(reflection.Strengths, 3)
	cleaned := make([]PromptImprovement, 0, min(len(reflection.Improvements), 3))
	for _, improvement := range reflection.Improvements {
		improvement.Issue = strings.TrimSpace(improvement.Issue)
		improvement.BetterPrompt = strings.TrimSpace(improvement.BetterPrompt)
		improvement.ExpectedBenefit = strings.TrimSpace(improvement.ExpectedBenefit)
		if improvement.Issue == "" || improvement.BetterPrompt == "" || improvement.ExpectedBenefit == "" {
			continue
		}
		cleaned = append(cleaned, improvement)
		if len(cleaned) == 3 {
			break
		}
	}
	reflection.Improvements = cleaned
	return reflection
}

func cleanStrings(values []string, limit int) []string {
	cleaned := make([]string, 0, min(len(values), limit))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			cleaned = append(cleaned, value)
			if len(cleaned) == limit {
				break
			}
		}
	}
	return cleaned
}

func journalContainsMarker(ctx context.Context, reader journalReader, path, marker string) (bool, error) {
	content, err := reader.ReadFile(ctx, path)
	if err == nil {
		return strings.Contains(content, marker), nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, err
}

func extractJSONObject(raw string) string {
	start := strings.IndexByte(raw, '{')
	if start < 0 {
		return raw
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(raw); i++ {
		ch := raw[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch ch {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}

		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return strings.TrimSpace(raw[start : i+1])
			}
		}
	}
	return raw
}

// FormatDistilledEntry renders observations as one combined journal entry.
func FormatDistilledEntry(observations []Observation, conversationID string) string {
	var sb strings.Builder
	sb.WriteString("(source: distiller, conversation: " + conversationID + ")\n")
	for _, obs := range observations {
		sb.WriteString("\n**[" + obs.Kind + "]** " + obs.Content + "\n")
	}
	return sb.String()
}

// FormatPromptReflectionEntry renders prompt feedback as a separate journal
// entry so it can be retained and analyzed independently from project facts.
func FormatPromptReflectionEntry(reflection PromptReflection, conversationID string) string {
	var sb strings.Builder
	sb.WriteString("(source: distiller, conversation: " + conversationID + ")\n")
	sb.WriteString(fmt.Sprintf("\n**Prompt effectiveness:** %d/10\n", reflection.QualityScore))
	for _, strength := range reflection.Strengths {
		sb.WriteString("\n**Strength:** " + strength + "\n")
	}
	for _, improvement := range reflection.Improvements {
		sb.WriteString("\n**Issue:** " + improvement.Issue + "\n")
		sb.WriteString("\n**Better prompt:** " + improvement.BetterPrompt + "\n")
		sb.WriteString("\n**Expected benefit:** " + improvement.ExpectedBenefit + "\n")
	}
	return sb.String()
}

// FlattenTranscript renders a captured request/response pair as plain text
// for the distiller prompt. The request body's messages array carries the full
// session history; the response body adds the final assistant turn. Individual
// messages are capped, and the whole transcript is tail-truncated to maxTokens
// (late turns carry resolutions).
func FlattenTranscript(requestBody, responseBody []byte, maxTokens int) string {
	const perMessageRunes = 2000

	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}

	var sb strings.Builder
	if err := json.Unmarshal(requestBody, &req); err == nil {
		for _, msg := range req.Messages {
			text := strings.TrimSpace(contentText(msg.Content))
			if text == "" {
				continue
			}
			if runes := []rune(text); len(runes) > perMessageRunes {
				text = string(runes[:perMessageRunes]) + " …[truncated]"
			}
			sb.WriteString(strings.ToUpper(msg.Role) + ": " + text + "\n\n")
		}
	}

	if finalTurn := extractResponseText(responseBody); finalTurn != "" {
		if runes := []rune(finalTurn); len(runes) > perMessageRunes {
			finalTurn = string(runes[:perMessageRunes]) + " …[truncated]"
		}
		sb.WriteString("ASSISTANT (final): " + finalTurn + "\n")
	}

	return TruncateToTokens(sb.String(), maxTokens, true)
}

// FlattenPiSessionJSONL renders a canonical Pi session JSONL file as plain text
// for the distiller prompt. It keeps only user and assistant message turns and
// their text blocks (skipping the session header, model/thinking-level changes,
// thinking blocks, and tool calls/results), caps each turn, and tail-truncates
// the whole transcript to maxTokens (late turns carry resolutions).
func FlattenPiSessionJSONL(raw []byte, maxTokens int) string {
	const perMessageRunes = 2000

	var sb strings.Builder
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var entry struct {
			Type    string          `json:"type"`
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type != "message" {
			continue
		}
		role := entry.Message.Role
		content := entry.Message.Content
		if role == "" {
			role = entry.Role
		}
		if len(content) == 0 {
			content = entry.Content
		}
		if role != "user" && role != "assistant" {
			continue
		}
		text := strings.TrimSpace(contentText(content))
		if text == "" {
			continue
		}
		if runes := []rune(text); len(runes) > perMessageRunes {
			text = string(runes[:perMessageRunes]) + " …[truncated]"
		}
		sb.WriteString(strings.ToUpper(role) + ": " + text + "\n\n")
	}

	return TruncateToTokens(sb.String(), maxTokens, true)
}

// contentText extracts text from a string-or-blocks message content field.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		parts := make([]string, 0, len(blocks))
		for _, b := range blocks {
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// extractResponseText pulls assistant text from a non-streaming JSON response
// (anthropic or openai shape). SSE responses are skipped — the request body of
// the *next* turn carries that text anyway.
func extractResponseText(body []byte) string {
	var anthropic struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(body, &anthropic); err == nil && len(anthropic.Content) > 0 {
		var parts []string
		for _, b := range anthropic.Content {
			if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}

	var openai struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &openai); err == nil && len(openai.Choices) > 0 {
		return openai.Choices[0].Message.Content
	}
	return ""
}
