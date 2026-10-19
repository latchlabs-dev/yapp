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
)

const (
	DefaultCompactionQueueName = "memory_compaction"
	CompactionTaskName         = "memory.compact_project"
)

// CompactionQueueConfig tunes the durable queue used by memory compaction.
type CompactionQueueConfig struct {
	DatabaseURL    string
	DriverName     string
	QueueName      string
	Workers        int
	PollInterval   time.Duration
	ClaimTimeout   time.Duration
	DedupInterval  time.Duration
	MaxAttempts    int
	PeakWindowsUTC string
}

func (c CompactionQueueConfig) withDefaults() CompactionQueueConfig {
	if c.DriverName == "" {
		c.DriverName = "pgx"
	}
	if c.QueueName == "" {
		c.QueueName = DefaultCompactionQueueName
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
	if c.DedupInterval <= 0 {
		c.DedupInterval = 6 * time.Hour
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	if strings.TrimSpace(c.PeakWindowsUTC) == "" {
		c.PeakWindowsUTC = DefaultCompactionPeakWindowsUTC
	}
	return c
}

type CompactionTaskParams struct {
	Project       string    `json:"project"`
	RequestedAt   time.Time `json:"requested_at"`
	EarliestRunAt time.Time `json:"earliest_run_at"`
}

type CompactionTaskResult struct {
	Project      string    `json:"project"`
	Compacted    bool      `json:"compacted"`
	BeforeTokens int       `json:"before_tokens,omitempty"`
	RanAt        time.Time `json:"ran_at"`
}

// CompactionQueue durably schedules project memory compaction with Absurd.
type CompactionQueue struct {
	app       *absurd.Client
	db        *sql.DB
	compactor *Compactor
	cfg       CompactionQueueConfig
	schedule  OffPeakSchedule
	task      absurd.TaskDefinition[CompactionTaskParams, CompactionTaskResult]
	now       func() time.Time
	logger    zerolog.Logger
}

func NewCompactionQueue(ctx context.Context, compactor *Compactor, cfg CompactionQueueConfig, logger zerolog.Logger) (*CompactionQueue, error) {
	if compactor == nil {
		return nil, fmt.Errorf("compactor is required")
	}
	cfg = cfg.withDefaults()
	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		return nil, fmt.Errorf("database URL is required")
	}
	schedule, err := NewOffPeakSchedule(cfg.PeakWindowsUTC)
	if err != nil {
		return nil, err
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

	q := &CompactionQueue{
		app:       app,
		db:        db,
		compactor: compactor,
		cfg:       cfg,
		schedule:  schedule,
		now:       time.Now,
		logger:    logger,
	}
	q.task = absurd.Task(
		CompactionTaskName,
		q.runTask,
		absurd.TaskOptions{
			QueueName:          cfg.QueueName,
			DefaultMaxAttempts: cfg.MaxAttempts,
			DefaultCancellation: &absurd.CancellationPolicy{
				MaxDuration: int64((24 * time.Hour).Seconds()),
			},
		},
	)
	if err := app.Register(q.task); err != nil {
		_ = q.Close()
		return nil, err
	}

	if err := app.CreateQueue(ctx, cfg.QueueName, absurd.CreateQueueOptions{
		QueuePolicyOptions: absurd.QueuePolicyOptions{CleanupTTL: "30 days"},
	}); err != nil {
		_ = q.Close()
		return nil, fmt.Errorf("ensure absurd queue %q: %w", cfg.QueueName, err)
	}

	return q, nil
}

func (q *CompactionQueue) Close() error {
	if q == nil || q.app == nil {
		return nil
	}
	return errors.Join(q.app.Close(), q.db.Close())
}

func (q *CompactionQueue) Start(ctx context.Context) {
	if q == nil {
		return
	}
	fatalOnLeaseTimeout := false
	q.logger.Info().
		Str("queue", q.cfg.QueueName).
		Int("workers", q.cfg.Workers).
		Dur("poll_interval", q.cfg.PollInterval).
		Dur("claim_timeout", q.cfg.ClaimTimeout).
		Str("peak_windows_utc", q.cfg.PeakWindowsUTC).
		Msg("memory compaction queue worker started")

	err := q.app.RunWorker(ctx, absurd.WorkerOptions{
		Concurrency:         q.cfg.Workers,
		BatchSize:           q.cfg.Workers,
		PollInterval:        q.cfg.PollInterval,
		ClaimTimeout:        q.cfg.ClaimTimeout,
		FatalOnLeaseTimeout: &fatalOnLeaseTimeout,
		OnError: func(err error) {
			q.logger.Warn().Err(err).Msg("memory compaction queue worker error")
		},
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		q.logger.Error().Err(err).Msg("memory compaction queue worker stopped")
	}
}

func (q *CompactionQueue) EnqueueProject(ctx context.Context, project string) error {
	if q == nil {
		return fmt.Errorf("compaction queue is nil")
	}
	project, err := SanitizeProject(project)
	if err != nil {
		return err
	}
	now := q.now().UTC()
	earliestRunAt := q.schedule.NextAllowedAt(now)
	bucket := q.schedule.ExecutionBucket(now, q.cfg.DedupInterval)
	spawned, err := q.task.Spawn(ctx, q.app, CompactionTaskParams{
		Project:       project,
		RequestedAt:   now,
		EarliestRunAt: earliestRunAt,
	}, absurd.SpawnOptions{
		IdempotencyKey: q.idempotencyKey(project, bucket),
		MaxAttempts:    q.cfg.MaxAttempts,
		RetryStrategy: &absurd.RetryStrategy{
			Kind:        "exponential",
			BaseSeconds: 30,
			Factor:      2,
			MaxSeconds:  1800,
		},
	})
	if err != nil {
		return fmt.Errorf("spawn compaction task: %w", err)
	}

	event := q.logger.Info()
	if !spawned.Created {
		event = q.logger.Debug()
	}
	event.
		Str("project", project).
		Str("task_id", spawned.TaskID).
		Bool("created", spawned.Created).
		Time("earliest_run_at", earliestRunAt).
		Msg("project memory compaction queued")
	return nil
}

func (q *CompactionQueue) runTask(ctx context.Context, params CompactionTaskParams) (CompactionTaskResult, error) {
	project, err := SanitizeProject(params.Project)
	if err != nil {
		return CompactionTaskResult{}, err
	}

	now := q.now().UTC()
	if wakeAt := q.schedule.NextAllowedAt(now); wakeAt.After(now) {
		q.logger.Info().
			Str("project", project).
			Time("wake_at", wakeAt).
			Msg("project memory compaction deferred until off-peak")
		if err := absurd.SleepUntil(ctx, "wait-for-offpeak", wakeAt); err != nil {
			return CompactionTaskResult{}, err
		}
	}

	beforeTokens, needed, err := q.compactor.NeedsCompaction(ctx, project)
	if err != nil {
		return CompactionTaskResult{}, err
	}
	if !needed {
		return CompactionTaskResult{
			Project:      project,
			Compacted:    false,
			BeforeTokens: beforeTokens,
			RanAt:        q.now().UTC(),
		}, nil
	}
	if err := q.compactor.CompactProject(ctx, project); err != nil {
		return CompactionTaskResult{}, err
	}
	return CompactionTaskResult{
		Project:      project,
		Compacted:    true,
		BeforeTokens: beforeTokens,
		RanAt:        q.now().UTC(),
	}, nil
}

func (q *CompactionQueue) idempotencyKey(project string, bucket time.Time) string {
	return fmt.Sprintf("%s:%s:%s", CompactionTaskName, project, bucket.UTC().Format(time.RFC3339))
}

type absurdZerolog struct {
	logger zerolog.Logger
}

func (l absurdZerolog) Printf(format string, args ...any) {
	l.logger.Debug().Msgf(format, args...)
}
