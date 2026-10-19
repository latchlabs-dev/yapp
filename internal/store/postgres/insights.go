package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/earlgrey/piproxy/internal/id"
)

type InsightProcessorRecord struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	GRPCTarget    string    `json:"grpc_target"`
	Plaintext     bool      `json:"plaintext"`
	TLSServerName string    `json:"tls_server_name,omitempty"`
	TimeoutMS     int       `json:"timeout_ms"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type InsightConfigurationRecord struct {
	ID                      string                  `json:"id"`
	ProcessorID             string                  `json:"processor_id"`
	Name                    string                  `json:"name"`
	TenantID                string                  `json:"tenant_id,omitempty"`
	ProjectID               string                  `json:"project_id,omitempty"`
	Instructions            string                  `json:"instructions"`
	AllowNewTags            bool                    `json:"allow_new_tags"`
	IncludeSessionSummary   bool                    `json:"include_session_summary"`
	IncludeInsightSummaries bool                    `json:"include_insight_summaries"`
	MaxInputItems           int                     `json:"max_input_items"`
	MaxInputChars           int                     `json:"max_input_chars"`
	Revision                int                     `json:"revision"`
	Status                  string                  `json:"status"`
	CreatedAt               time.Time               `json:"created_at"`
	UpdatedAt               time.Time               `json:"updated_at"`
	Processor               *InsightProcessorRecord `json:"processor,omitempty"`
}

type InsightTagRecord struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	UsageCount  uint64    `json:"usage_count"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type InsightRunRecord struct {
	ID                    string          `json:"id"`
	ConfigurationID       string          `json:"configuration_id"`
	ConfigurationName     string          `json:"configuration_name,omitempty"`
	ConfigurationRevision int             `json:"configuration_revision"`
	SessionSubmissionID   string          `json:"session_submission_id"`
	TenantID              string          `json:"tenant_id"`
	OwnerID               string          `json:"-"`
	ConversationID        string          `json:"conversation_id"`
	ProjectID             string          `json:"project_id"`
	InputSHA256           string          `json:"input_sha256"`
	Status                string          `json:"status"`
	SessionSummary        string          `json:"session_summary"`
	Usage                 json.RawMessage `json:"usage"`
	Warnings              json.RawMessage `json:"warnings"`
	Error                 string          `json:"error,omitempty"`
	InsightCount          int             `json:"insight_count"`
	StartedAt             time.Time       `json:"started_at"`
	CompletedAt           *time.Time      `json:"completed_at,omitempty"`
}

type InsightRecord struct {
	ID            string          `json:"id"`
	RunID         string          `json:"run_id"`
	Summary       string          `json:"summary"`
	Content       string          `json:"content"`
	Tags          []string        `json:"tags"`
	SourceItemIDs []string        `json:"source_item_ids"`
	Metadata      json.RawMessage `json:"metadata"`
	CreatedAt     time.Time       `json:"created_at"`
}

type InsightStore struct{ pool *Pool }

func NewInsightStore(pool *Pool) *InsightStore { return &InsightStore{pool: pool} }

func (s *InsightStore) ListProcessors(ctx context.Context) ([]*InsightProcessorRecord, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, grpc_target, plaintext, COALESCE(tls_server_name, ''), timeout_ms,
		       status, created_at, updated_at
		  FROM insight_processors ORDER BY lower(name), id`)
	if err != nil {
		return nil, fmt.Errorf("list insight processors: %w", err)
	}
	defer rows.Close()
	var records []*InsightProcessorRecord
	for rows.Next() {
		r := &InsightProcessorRecord{}
		if err := rows.Scan(&r.ID, &r.Name, &r.GRPCTarget, &r.Plaintext, &r.TLSServerName, &r.TimeoutMS, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan insight processor: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *InsightStore) GetProcessor(ctx context.Context, processorID string) (*InsightProcessorRecord, error) {
	r := &InsightProcessorRecord{}
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, grpc_target, plaintext, COALESCE(tls_server_name, ''), timeout_ms,
		       status, created_at, updated_at
		  FROM insight_processors WHERE id = $1`, processorID).Scan(
		&r.ID, &r.Name, &r.GRPCTarget, &r.Plaintext, &r.TLSServerName, &r.TimeoutMS, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("get insight processor: %w", err)
	}
	return r, nil
}

func (s *InsightStore) CreateProcessor(ctx context.Context, r *InsightProcessorRecord) error {
	if r.ID == "" {
		r.ID = id.New("iprc")
	}
	if r.TimeoutMS == 0 {
		r.TimeoutMS = 120000
	}
	if r.Status == "" {
		r.Status = "active"
	}
	return scanProcessor(s.pool.QueryRow(ctx, `
		INSERT INTO insight_processors (id, name, grpc_target, plaintext, tls_server_name, timeout_ms, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id, name, grpc_target, plaintext, COALESCE(tls_server_name, ''), timeout_ms, status, created_at, updated_at`,
		r.ID, r.Name, r.GRPCTarget, r.Plaintext, nullableText(r.TLSServerName), r.TimeoutMS, r.Status), r)
}

func (s *InsightStore) UpdateProcessor(ctx context.Context, r *InsightProcessorRecord) error {
	return scanProcessor(s.pool.QueryRow(ctx, `
		UPDATE insight_processors SET name=$2, grpc_target=$3, plaintext=$4, tls_server_name=$5,
		       timeout_ms=$6, status=$7, updated_at=now() WHERE id=$1
		RETURNING id, name, grpc_target, plaintext, COALESCE(tls_server_name, ''), timeout_ms, status, created_at, updated_at`,
		r.ID, r.Name, r.GRPCTarget, r.Plaintext, nullableText(r.TLSServerName), r.TimeoutMS, r.Status), r)
}

func (s *InsightStore) DisableProcessor(ctx context.Context, processorID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE insight_processors SET status='disabled', updated_at=now() WHERE id=$1`, processorID)
	if err != nil {
		return fmt.Errorf("disable insight processor: %w", err)
	}
	return nil
}

type rowScanner interface{ Scan(...any) error }

func scanProcessor(row rowScanner, r *InsightProcessorRecord) error {
	if err := row.Scan(&r.ID, &r.Name, &r.GRPCTarget, &r.Plaintext, &r.TLSServerName, &r.TimeoutMS, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return fmt.Errorf("write insight processor: %w", err)
	}
	return nil
}

func (s *InsightStore) ListConfigurations(ctx context.Context) ([]*InsightConfigurationRecord, error) {
	return s.listConfigurations(ctx, "", "", false)
}

func (s *InsightStore) ListMatchingConfigurations(ctx context.Context, tenantID, projectID string) ([]*InsightConfigurationRecord, error) {
	return s.listConfigurations(ctx, tenantID, projectID, true)
}

func (s *InsightStore) listConfigurations(ctx context.Context, tenantID, projectID string, matching bool) ([]*InsightConfigurationRecord, error) {
	where := ""
	args := []any{}
	if matching {
		where = `WHERE ic.status='active' AND ip.status='active'
		           AND (ic.tenant_id IS NULL OR ic.tenant_id=$1)
		           AND (ic.project_id IS NULL OR ic.project_id=$2)`
		args = append(args, tenantID, projectID)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ic.id, ic.processor_id, ic.name, COALESCE(ic.tenant_id, ''), COALESCE(ic.project_id, ''),
		       ic.instructions, ic.allow_new_tags, ic.include_session_summary, ic.include_insight_summaries,
		       ic.max_input_items, ic.max_input_chars, ic.revision, ic.status, ic.created_at, ic.updated_at,
		       ip.id, ip.name, ip.grpc_target, ip.plaintext, COALESCE(ip.tls_server_name, ''), ip.timeout_ms,
		       ip.status, ip.created_at, ip.updated_at
		  FROM insight_configurations ic JOIN insight_processors ip ON ip.id=ic.processor_id
		`+where+` ORDER BY lower(ic.name), ic.id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list insight configurations: %w", err)
	}
	defer rows.Close()
	var records []*InsightConfigurationRecord
	for rows.Next() {
		r := &InsightConfigurationRecord{Processor: &InsightProcessorRecord{}}
		if err := rows.Scan(
			&r.ID, &r.ProcessorID, &r.Name, &r.TenantID, &r.ProjectID,
			&r.Instructions, &r.AllowNewTags, &r.IncludeSessionSummary, &r.IncludeInsightSummaries,
			&r.MaxInputItems, &r.MaxInputChars, &r.Revision, &r.Status, &r.CreatedAt, &r.UpdatedAt,
			&r.Processor.ID, &r.Processor.Name, &r.Processor.GRPCTarget, &r.Processor.Plaintext,
			&r.Processor.TLSServerName, &r.Processor.TimeoutMS, &r.Processor.Status,
			&r.Processor.CreatedAt, &r.Processor.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan insight configuration: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *InsightStore) GetConfiguration(ctx context.Context, configurationID string) (*InsightConfigurationRecord, error) {
	rows, err := s.listConfigurations(ctx, "", "", false)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		if r.ID == configurationID {
			return r, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (s *InsightStore) CreateConfiguration(ctx context.Context, r *InsightConfigurationRecord) error {
	if r.ID == "" {
		r.ID = id.New("icfg")
	}
	configurationDefaults(r)
	return scanConfiguration(s.pool.QueryRow(ctx, `
		INSERT INTO insight_configurations
		(id, processor_id, name, tenant_id, project_id, instructions, allow_new_tags,
		 include_session_summary, include_insight_summaries, max_input_items, max_input_chars, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		RETURNING id, processor_id, name, COALESCE(tenant_id, ''), COALESCE(project_id, ''), instructions,
		 allow_new_tags, include_session_summary, include_insight_summaries, max_input_items,
		 max_input_chars, revision, status, created_at, updated_at`,
		r.ID, r.ProcessorID, r.Name, nullableText(r.TenantID), nullableText(r.ProjectID), r.Instructions,
		r.AllowNewTags, r.IncludeSessionSummary, r.IncludeInsightSummaries, r.MaxInputItems, r.MaxInputChars, r.Status), r)
}

func (s *InsightStore) UpdateConfiguration(ctx context.Context, r *InsightConfigurationRecord) error {
	configurationDefaults(r)
	return scanConfiguration(s.pool.QueryRow(ctx, `
		UPDATE insight_configurations SET processor_id=$2, name=$3, tenant_id=$4, project_id=$5,
		 instructions=$6, allow_new_tags=$7, include_session_summary=$8, include_insight_summaries=$9,
		 max_input_items=$10, max_input_chars=$11, status=$12, revision=revision+1, updated_at=now()
		 WHERE id=$1
		RETURNING id, processor_id, name, COALESCE(tenant_id, ''), COALESCE(project_id, ''), instructions,
		 allow_new_tags, include_session_summary, include_insight_summaries, max_input_items,
		 max_input_chars, revision, status, created_at, updated_at`,
		r.ID, r.ProcessorID, r.Name, nullableText(r.TenantID), nullableText(r.ProjectID), r.Instructions,
		r.AllowNewTags, r.IncludeSessionSummary, r.IncludeInsightSummaries, r.MaxInputItems, r.MaxInputChars, r.Status), r)
}

func configurationDefaults(r *InsightConfigurationRecord) {
	if r.MaxInputItems == 0 {
		r.MaxInputItems = 1000
	}
	if r.MaxInputChars == 0 {
		r.MaxInputChars = 500000
	}
	if r.Status == "" {
		r.Status = "active"
	}
}

func scanConfiguration(row rowScanner, r *InsightConfigurationRecord) error {
	if err := row.Scan(&r.ID, &r.ProcessorID, &r.Name, &r.TenantID, &r.ProjectID, &r.Instructions,
		&r.AllowNewTags, &r.IncludeSessionSummary, &r.IncludeInsightSummaries,
		&r.MaxInputItems, &r.MaxInputChars, &r.Revision, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return fmt.Errorf("write insight configuration: %w", err)
	}
	return nil
}

func (s *InsightStore) DisableConfiguration(ctx context.Context, configurationID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE insight_configurations SET status='disabled', revision=revision+1, updated_at=now() WHERE id=$1`, configurationID)
	if err != nil {
		return fmt.Errorf("disable insight configuration: %w", err)
	}
	return nil
}

func (s *InsightStore) ListTags(ctx context.Context) ([]*InsightTagRecord, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, description, usage_count, status, created_at, updated_at FROM insight_tags ORDER BY usage_count DESC, name`)
	if err != nil {
		return nil, fmt.Errorf("list insight tags: %w", err)
	}
	defer rows.Close()
	var records []*InsightTagRecord
	for rows.Next() {
		r := &InsightTagRecord{}
		if err := rows.Scan(&r.Name, &r.Description, &r.UsageCount, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan insight tag: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func (s *InsightStore) UpsertTag(ctx context.Context, r *InsightTagRecord) error {
	r.Name = normalizeInsightTag(r.Name)
	if r.Status == "" {
		r.Status = "active"
	}
	return s.pool.QueryRow(ctx, `
		INSERT INTO insight_tags (name, description, status) VALUES ($1,$2,$3)
		ON CONFLICT (name) DO UPDATE SET description=EXCLUDED.description, status=EXCLUDED.status, updated_at=now()
		RETURNING name, description, usage_count, status, created_at, updated_at`,
		r.Name, r.Description, r.Status).Scan(&r.Name, &r.Description, &r.UsageCount, &r.Status, &r.CreatedAt, &r.UpdatedAt)
}

func (s *InsightStore) DisableTag(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `UPDATE insight_tags SET status='disabled', updated_at=now() WHERE name=$1`, normalizeInsightTag(name))
	if err != nil {
		return fmt.Errorf("disable insight tag: %w", err)
	}
	return nil
}

func (s *InsightStore) CreateRun(ctx context.Context, r *InsightRunRecord) (bool, error) {
	if r.ID == "" {
		r.ID = id.New("irun")
	}
	var existingID, existingStatus string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO insight_runs
		(id, configuration_id, configuration_revision, session_submission_id, tenant_id, owner_id,
		 conversation_id, project_id, input_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT (configuration_id, configuration_revision, session_submission_id, input_sha256)
		DO UPDATE SET status=CASE WHEN insight_runs.status='failed' THEN 'running' ELSE insight_runs.status END,
		              error=CASE WHEN insight_runs.status='failed' THEN NULL ELSE insight_runs.error END,
		              completed_at=CASE WHEN insight_runs.status='failed' THEN NULL ELSE insight_runs.completed_at END
		RETURNING id, status`, r.ID, r.ConfigurationID, r.ConfigurationRevision, r.SessionSubmissionID,
		r.TenantID, r.OwnerID, r.ConversationID, r.ProjectID, r.InputSHA256).Scan(&existingID, &existingStatus)
	if err != nil {
		return false, fmt.Errorf("create insight run: %w", err)
	}
	created := existingID == r.ID
	r.ID, r.Status = existingID, existingStatus
	return created, nil
}

func (s *InsightStore) GetRunResults(ctx context.Context, runID string) (*InsightRunRecord, []*InsightRecord, error) {
	r := &InsightRunRecord{}
	err := s.pool.QueryRow(ctx, `
		SELECT ir.id, ir.configuration_id, ic.name, ir.configuration_revision, ir.session_submission_id,
		       ir.tenant_id, ir.owner_id, ir.conversation_id, ir.project_id, ir.input_sha256,
		       ir.status, ir.session_summary, ir.usage, ir.warnings, COALESCE(ir.error, ''),
		       ir.started_at, ir.completed_at
		  FROM insight_runs ir JOIN insight_configurations ic ON ic.id=ir.configuration_id
		 WHERE ir.id=$1`, runID).Scan(&r.ID, &r.ConfigurationID, &r.ConfigurationName,
		&r.ConfigurationRevision, &r.SessionSubmissionID, &r.TenantID, &r.OwnerID,
		&r.ConversationID, &r.ProjectID, &r.InputSHA256, &r.Status, &r.SessionSummary,
		&r.Usage, &r.Warnings, &r.Error, &r.StartedAt, &r.CompletedAt)
	if err != nil {
		return nil, nil, fmt.Errorf("get insight run: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, run_id, summary, content, tags, source_item_ids, metadata, created_at
		  FROM insights WHERE run_id=$1 ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, nil, fmt.Errorf("list insight results: %w", err)
	}
	defer rows.Close()
	var results []*InsightRecord
	for rows.Next() {
		result := &InsightRecord{}
		if err := rows.Scan(&result.ID, &result.RunID, &result.Summary, &result.Content,
			&result.Tags, &result.SourceItemIDs, &result.Metadata, &result.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("scan insight result: %w", err)
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	r.InsightCount = len(results)
	return r, results, nil
}

func (s *InsightStore) CompleteRun(ctx context.Context, runID, sessionSummary string, results []*InsightRecord, proposedTags []*InsightTagRecord, usage, warnings json.RawMessage) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin insight completion: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, tag := range proposedTags {
		name := normalizeInsightTag(tag.Name)
		if name == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO insight_tags (name, description) VALUES ($1,$2)
			ON CONFLICT (name) DO UPDATE SET description=CASE WHEN insight_tags.description='' THEN EXCLUDED.description ELSE insight_tags.description END,
			updated_at=now()`, name, strings.TrimSpace(tag.Description)); err != nil {
			return fmt.Errorf("upsert proposed insight tag: %w", err)
		}
	}
	for _, result := range results {
		if result.ID == "" {
			result.ID = id.New("ins")
		}
		if len(result.Metadata) == 0 {
			result.Metadata = json.RawMessage(`{}`)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO insights (id, run_id, summary, content, tags, source_item_ids, metadata)
			VALUES ($1,$2,$3,$4,$5,$6,$7) ON CONFLICT (id) DO NOTHING`,
			result.ID, runID, result.Summary, result.Content, result.Tags, result.SourceItemIDs, result.Metadata); err != nil {
			return fmt.Errorf("insert insight: %w", err)
		}
		for _, rawTag := range result.Tags {
			name := normalizeInsightTag(rawTag)
			if name == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO insight_tags (name, usage_count) VALUES ($1,1)
				ON CONFLICT (name) DO UPDATE SET usage_count=insight_tags.usage_count+1, updated_at=now()`, name); err != nil {
				return fmt.Errorf("increment insight tag usage: %w", err)
			}
		}
	}
	if len(usage) == 0 {
		usage = json.RawMessage(`{}`)
	}
	if len(warnings) == 0 {
		warnings = json.RawMessage(`[]`)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE insight_runs SET status='completed', session_summary=$2, usage=$3, warnings=$4,
		       error=NULL, completed_at=now() WHERE id=$1 AND status='running'`, runID, sessionSummary, usage, warnings)
	if err != nil {
		return fmt.Errorf("complete insight run: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return errors.New("insight run was not running")
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit insight run: %w", err)
	}
	return nil
}

func (s *InsightStore) FailRun(ctx context.Context, runID, runErr string) error {
	_, err := s.pool.Exec(ctx, `UPDATE insight_runs SET status='failed', error=$2, completed_at=now() WHERE id=$1`, runID, nullableText(runErr))
	if err != nil {
		return fmt.Errorf("fail insight run: %w", err)
	}
	return nil
}

func (s *InsightStore) ListRuns(ctx context.Context, limit int) ([]*InsightRunRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT ir.id, ir.configuration_id, ic.name, ir.configuration_revision, ir.session_submission_id,
		       ir.tenant_id, ir.owner_id, ir.conversation_id, ir.project_id, ir.input_sha256,
		       ir.status, ir.session_summary, ir.usage, ir.warnings, COALESCE(ir.error, ''),
		       COUNT(i.id), ir.started_at, ir.completed_at
		  FROM insight_runs ir
		  JOIN insight_configurations ic ON ic.id=ir.configuration_id
		  LEFT JOIN insights i ON i.run_id=ir.id
		 GROUP BY ir.id, ic.name
		 ORDER BY ir.started_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list insight runs: %w", err)
	}
	defer rows.Close()
	var records []*InsightRunRecord
	for rows.Next() {
		r := &InsightRunRecord{}
		if err := rows.Scan(&r.ID, &r.ConfigurationID, &r.ConfigurationName, &r.ConfigurationRevision,
			&r.SessionSubmissionID, &r.TenantID, &r.OwnerID, &r.ConversationID, &r.ProjectID,
			&r.InputSHA256, &r.Status, &r.SessionSummary, &r.Usage, &r.Warnings, &r.Error,
			&r.InsightCount, &r.StartedAt, &r.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan insight run: %w", err)
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

func normalizeInsightTag(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), "-")
}
