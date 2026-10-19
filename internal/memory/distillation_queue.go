package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/earendil-works/absurd/sdks/go/absurd"
	"github.com/rs/zerolog"

	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

const (
	DefaultDistillationQueueName  = "memory_distillation"
	DistillationTaskName          = "memory.distill_session"
	DefaultDistillationNightlyUTC = "23:00"
	DefaultDistillationIdleAfter  = 15 * time.Minute
	distillationReconcileBatch    = 100
)

// DistillationQueueConfig tunes the durable nightly session-synthesis queue.
type DistillationQueueConfig struct {
	DatabaseURL       string
	DriverName        string
	QueueName         string
	Workers           int
	PollInterval      time.Duration
	ClaimTimeout      time.Duration
	MaxAttempts       int
	NightlyAtUTC      string
	IdleAfter         time.Duration
	AllowedProjectIDs []string
}

func (c DistillationQueueConfig) withDefaults() DistillationQueueConfig {
	if c.DriverName == "" {
		c.DriverName = "pgx"
	}
	if c.QueueName == "" {
		c.QueueName = DefaultDistillationQueueName
	}
	if c.Workers <= 0 {
		c.Workers = 1
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 30 * time.Second
	}
	if c.ClaimTimeout <= 0 {
		c.ClaimTimeout = 30 * time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if strings.TrimSpace(c.NightlyAtUTC) == "" {
		c.NightlyAtUTC = DefaultDistillationNightlyUTC
	}
	if c.IdleAfter <= 0 {
		c.IdleAfter = DefaultDistillationIdleAfter
	}
	return c
}

type DistillationTaskParams struct {
	TenantID       string    `json:"tenant_id"`
	OwnerID        string    `json:"owner_id"`
	ConversationID string    `json:"conversation_id"`
	RequestedAt    time.Time `json:"requested_at"`
	ScheduledFor   time.Time `json:"scheduled_for"`
}

type DistillationTaskResult struct {
	ConversationID           string    `json:"conversation_id"`
	Project                  string    `json:"project,omitempty"`
	AlreadyDistilled         bool      `json:"already_distilled"`
	SkippedDisallowedProject bool      `json:"skipped_disallowed_project,omitempty"`
	RanAt                    time.Time `json:"ran_at"`
}

type distillationTaskStore interface {
	GetByIdentity(ctx context.Context, identity pgstore.SessionIdentity) (*pgstore.SessionSubmissionRecord, []byte, error)
	ListUndistilledSessions(ctx context.Context, afterID string, limit int) ([]*pgstore.SessionSubmissionRecord, error)
	RecordDistillAttemptFor(ctx context.Context, identity pgstore.SessionIdentity) error
	RecordDistillErrorFor(ctx context.Context, identity pgstore.SessionIdentity, distillErr string) error
	TenantDistillationEnabled(ctx context.Context, tenantID string) (bool, error)
}

type sessionDistiller interface {
	DistillOne(ctx context.Context, rec *pgstore.SessionSubmissionRecord) error
}

type distillationProjectMatcher interface {
	AllowsProject(ctx context.Context, tenantID, projectID string) (bool, error)
}

// DistillationQueue durably schedules one nightly synthesis task per session.
// The task reads the session row at execution time, so repeated submissions
// before the nightly run update the source data without creating another task.
type DistillationQueue struct {
	app        *absurd.Client
	db         *sql.DB
	distiller  sessionDistiller
	store      distillationTaskStore
	cfg        DistillationQueueConfig
	allowed    map[string]struct{}
	nightly    time.Duration
	task       absurd.TaskDefinition[DistillationTaskParams, DistillationTaskResult]
	now        func() time.Time
	sleepUntil func(context.Context, string, time.Time) error
	logger     zerolog.Logger
}

func NewDistillationQueue(ctx context.Context, distiller sessionDistiller, store distillationTaskStore, cfg DistillationQueueConfig, logger zerolog.Logger) (*DistillationQueue, error) {
	if distiller == nil {
		return nil, fmt.Errorf("distiller is required")
	}
	if store == nil {
		return nil, fmt.Errorf("session store is required")
	}
	cfg = cfg.withDefaults()
	allowed := make(map[string]struct{}, len(cfg.AllowedProjectIDs))
	for _, projectID := range cfg.AllowedProjectIDs {
		if projectID = strings.TrimSpace(projectID); projectID != "" {
			allowed[projectID] = struct{}{}
		}
	}
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return nil, fmt.Errorf("database URL is required")
	}
	nightly, err := parseClockUTC(cfg.NightlyAtUTC, false)
	if err != nil {
		return nil, fmt.Errorf("invalid nightly UTC time %q: %w", cfg.NightlyAtUTC, err)
	}

	app, db, err := newQueueClient(absurd.Options{
		DriverName:         cfg.DriverName,
		DatabaseURL:        cfg.DatabaseURL,
		QueueName:          cfg.QueueName,
		DefaultMaxAttempts: cfg.MaxAttempts,
		Logger:             absurdZerolog{logger: logger},
	})
	if err != nil {
		return nil, err
	}

	q := &DistillationQueue{
		app:        app,
		db:         db,
		distiller:  distiller,
		store:      store,
		cfg:        cfg,
		allowed:    allowed,
		nightly:    nightly,
		now:        time.Now,
		sleepUntil: absurd.SleepUntil,
		logger:     logger,
	}
	q.task = absurd.Task(
		DistillationTaskName,
		q.runTask,
		absurd.TaskOptions{
			QueueName:          cfg.QueueName,
			DefaultMaxAttempts: cfg.MaxAttempts,
			DefaultCancellation: &absurd.CancellationPolicy{
				MaxDuration: int64((36 * time.Hour).Seconds()),
			},
		},
	)
	if err := app.Register(q.task); err != nil {
		_ = q.Close()
		return nil, err
	}
	if err := app.CreateQueue(ctx, cfg.QueueName, absurd.CreateQueueOptions{
		QueuePolicyOptions: absurd.QueuePolicyOptions{CleanupTTL: "90 days"},
	}); err != nil {
		_ = q.Close()
		return nil, fmt.Errorf("ensure absurd queue %q: %w", cfg.QueueName, err)
	}

	return q, nil
}

func (q *DistillationQueue) Close() error {
	if q == nil || q.app == nil {
		return nil
	}
	return errors.Join(q.app.Close(), q.db.Close())
}

func (q *DistillationQueue) Start(ctx context.Context) {
	if q == nil {
		return
	}
	fatalOnLeaseTimeout := false
	q.logger.Info().
		Str("queue", q.cfg.QueueName).
		Int("workers", q.cfg.Workers).
		Str("nightly_at_utc", q.cfg.NightlyAtUTC).
		Msg("memory distillation queue worker started")

	err := q.app.RunWorker(ctx, absurd.WorkerOptions{
		Concurrency:         q.cfg.Workers,
		BatchSize:           q.cfg.Workers,
		PollInterval:        q.cfg.PollInterval,
		ClaimTimeout:        q.cfg.ClaimTimeout,
		FatalOnLeaseTimeout: &fatalOnLeaseTimeout,
		OnError: func(err error) {
			q.logger.Warn().Err(err).Msg("memory distillation queue worker error")
		},
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		q.logger.Error().Err(err).Msg("memory distillation queue worker stopped")
	}
}

// AllowsProject reports whether a session has a resolved project and at least
// one matching database-backed insight configuration. The legacy environment
// allowlist remains an optional additional restriction when populated.
func (q *DistillationQueue) AllowsProject(ctx context.Context, tenantID, projectID string) (bool, error) {
	if q == nil {
		return false, nil
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return false, nil
	}
	if len(q.allowed) > 0 {
		if _, allowed := q.allowed[projectID]; !allowed {
			return false, nil
		}
	}
	if matcher, ok := q.distiller.(distillationProjectMatcher); ok {
		return matcher.AllowsProject(ctx, tenantID, projectID)
	}
	return true, nil
}

func (q *DistillationQueue) EnqueueSession(ctx context.Context, identity pgstore.SessionIdentity) error {
	if q == nil {
		return fmt.Errorf("distillation queue is nil")
	}
	identity.TenantID = strings.TrimSpace(identity.TenantID)
	identity.OwnerID = strings.TrimSpace(identity.OwnerID)
	identity.ConversationID = strings.TrimSpace(identity.ConversationID)
	if identity.TenantID == "" || identity.OwnerID == "" || identity.ConversationID == "" {
		return fmt.Errorf("tenant, owner, and conversation IDs are required")
	}
	now := q.now().UTC()
	scheduledFor := nextNightlyAt(now, q.nightly)
	spawned, err := q.task.Spawn(ctx, q.app, DistillationTaskParams{
		TenantID:       identity.TenantID,
		OwnerID:        identity.OwnerID,
		ConversationID: identity.ConversationID,
		RequestedAt:    now,
		ScheduledFor:   scheduledFor,
	}, absurd.SpawnOptions{
		IdempotencyKey: q.idempotencyKey(identity),
		MaxAttempts:    q.cfg.MaxAttempts,
		RetryStrategy: &absurd.RetryStrategy{
			Kind:        "exponential",
			BaseSeconds: 30,
			Factor:      2,
			MaxSeconds:  1800,
		},
	})
	if err != nil {
		return fmt.Errorf("spawn distillation task: %w", err)
	}

	event := q.logger.Info()
	if !spawned.Created {
		event = q.logger.Debug()
	}
	event.
		Str("conversation_id", identity.ConversationID).
		Str("tenant_id", identity.TenantID).
		Str("task_id", spawned.TaskID).
		Bool("created", spawned.Created).
		Time("scheduled_for", scheduledFor).
		Msg("session memory distillation queued")
	return nil
}

// ReconcileUndistilled ensures every existing undistilled submission has an
// Absurd task. Spawning is idempotent, so this safely covers both migrations
// from the old sweeper and submissions whose original enqueue was interrupted.
func (q *DistillationQueue) ReconcileUndistilled(ctx context.Context) (int, error) {
	if q == nil {
		return 0, fmt.Errorf("distillation queue is nil")
	}
	return reconcileUndistilled(ctx, q.store, q.AllowsProject, q.EnqueueSession)
}

func reconcileUndistilled(ctx context.Context, store distillationTaskStore, allowsProject func(context.Context, string, string) (bool, error), enqueue func(context.Context, pgstore.SessionIdentity) error) (int, error) {
	var after string
	reconciled := 0
	for {
		records, err := store.ListUndistilledSessions(ctx, after, distillationReconcileBatch)
		if err != nil {
			return reconciled, err
		}
		for _, record := range records {
			after = record.ID
			allowed := true
			if allowsProject != nil {
				allowed, err = allowsProject(ctx, record.TenantID, record.ProjectID)
				if err != nil {
					return reconciled, fmt.Errorf("match insight configuration for session %s: %w", record.ConversationID, err)
				}
			}
			if !allowed {
				continue
			}
			if err := enqueue(ctx, record.Identity()); err != nil {
				return reconciled, fmt.Errorf("enqueue undistilled session %s: %w", record.ConversationID, err)
			}
			reconciled++
		}
		if len(records) < distillationReconcileBatch {
			return reconciled, nil
		}
	}
}

func (q *DistillationQueue) runTask(ctx context.Context, params DistillationTaskParams) (DistillationTaskResult, error) {
	identity := pgstore.SessionIdentity{
		TenantID:       strings.TrimSpace(params.TenantID),
		OwnerID:        strings.TrimSpace(params.OwnerID),
		ConversationID: strings.TrimSpace(params.ConversationID),
	}
	if identity.TenantID == "" || identity.OwnerID == "" || identity.ConversationID == "" {
		return DistillationTaskResult{}, fmt.Errorf("tenant, owner, and conversation IDs are required")
	}
	if params.ScheduledFor.After(q.now().UTC()) {
		if err := q.waitUntil(ctx, "wait-for-nightly-run", params.ScheduledFor); err != nil {
			return DistillationTaskResult{}, err
		}
	}

	enabled, err := q.store.TenantDistillationEnabled(ctx, identity.TenantID)
	if err != nil {
		return DistillationTaskResult{}, err
	}
	if !enabled {
		return DistillationTaskResult{}, fmt.Errorf("tenant session distillation is disabled")
	}
	initialRecord, _, err := q.store.GetByIdentity(ctx, identity)
	if err != nil {
		return DistillationTaskResult{}, err
	}
	allowed, err := q.AllowsProject(ctx, initialRecord.TenantID, initialRecord.ProjectID)
	if err != nil {
		return DistillationTaskResult{}, err
	}
	if !allowed {
		q.logger.Info().
			Str("conversation_id", identity.ConversationID).
			Str("project", initialRecord.Project).
			Str("project_id", initialRecord.ProjectID).
			Msg("session memory distillation task skipped by project allowlist")
		return DistillationTaskResult{
			ConversationID:           identity.ConversationID,
			Project:                  initialRecord.Project,
			SkippedDisallowedProject: true,
			RanAt:                    q.now().UTC(),
		}, nil
	}
	record, err := q.waitForInactivity(ctx, identity)
	if err != nil {
		return DistillationTaskResult{}, err
	}
	result := DistillationTaskResult{
		ConversationID: identity.ConversationID,
		Project:        record.Project,
		RanAt:          q.now().UTC(),
	}
	if record.DistilledAt != nil {
		result.AlreadyDistilled = true
		return result, nil
	}
	if err := q.store.RecordDistillAttemptFor(ctx, identity); err != nil {
		return DistillationTaskResult{}, err
	}
	if err := q.distiller.DistillOne(ctx, record); err != nil {
		if recordErr := q.store.RecordDistillErrorFor(ctx, identity, err.Error()); recordErr != nil {
			return DistillationTaskResult{}, errors.Join(err, recordErr)
		}
		return DistillationTaskResult{}, err
	}
	return result, nil
}

func (q *DistillationQueue) waitForInactivity(ctx context.Context, identity pgstore.SessionIdentity) (*pgstore.SessionSubmissionRecord, error) {
	idleAfter := q.cfg.IdleAfter
	if idleAfter <= 0 {
		idleAfter = DefaultDistillationIdleAfter
	}
	for {
		record, _, err := q.store.GetByIdentity(ctx, identity)
		if err != nil {
			return nil, err
		}
		if record.DistilledAt != nil {
			return record, nil
		}

		now := q.now().UTC()
		eligibleAt := now.Add(idleAfter)
		if record.EndedAt != nil {
			eligibleAt = record.EndedAt.UTC().Add(idleAfter)
			if !eligibleAt.After(now) {
				return record, nil
			}
		}

		q.logger.Debug().
			Str("conversation_id", identity.ConversationID).
			Time("eligible_at", eligibleAt).
			Msg("session still active; deferring memory distillation")
		stepName := "wait-for-session-inactivity:" + eligibleAt.UTC().Format(time.RFC3339Nano)
		if err := q.waitUntil(ctx, stepName, eligibleAt); err != nil {
			return nil, err
		}
	}
}

func (q *DistillationQueue) waitUntil(ctx context.Context, stepName string, wakeAt time.Time) error {
	if q.sleepUntil != nil {
		return q.sleepUntil(ctx, stepName, wakeAt)
	}
	return absurd.SleepUntil(ctx, stepName, wakeAt)
}

func nextNightlyAt(now time.Time, nightly time.Duration) time.Time {
	now = now.UTC()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	next := dayStart.Add(nightly)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

func (q *DistillationQueue) idempotencyKey(identity pgstore.SessionIdentity) string {
	return DistillationTaskName + ":" + identity.TenantID + ":" + identity.OwnerID + ":" + identity.ConversationID
}
