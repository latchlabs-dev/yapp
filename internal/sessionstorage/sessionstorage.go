// Package sessionstorage wires the session archive — object store, raw
// capture store, transcript state store, builder and reader — from
// configuration, so the proxy, the admin API and the maintenance command
// construct them identically and agree on where bodies live.
package sessionstorage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/capture"
	"github.com/earlgrey/piproxy/internal/config"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// Services is the wired session archive.
type Services struct {
	Config   config.SessionArchiveConfig
	Objects  blob.Objects
	Payloads *pgstore.ConversationPayloadStore
	States   *pgstore.SessionArchiveStore
	Builder  *sessionarchive.Builder
	Reader   *sessionarchive.Reader
	Query    *sessionarchive.QueryClient

	logger       zerolog.Logger
	close        func() error
	baseCtx      context.Context
	repairCancel context.CancelFunc
	repairDone   chan struct{}
	repairStart  sync.Once
}

// rebuildTimeout bounds one background rebuild; the largest conversations
// (1,000+ requests) read every raw body.
const rebuildTimeout = 30 * time.Minute

// Open builds the session archive over pool. ctx bounds background rebuilds
// started through StartRebuild. Callers must Close it.
func Open(ctx context.Context, cfg config.SessionArchiveConfig, pool *pgstore.Pool, logger zerolog.Logger) (*Services, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	var objects blob.Objects
	closeFn := func() error { return nil }
	switch cfg.ObjectsBackend {
	case config.SessionObjectsGCS:
		gcs, err := blob.NewGCS(ctx, cfg.ObjectsBucket, cfg.ObjectsPrefix)
		if err != nil {
			return nil, fmt.Errorf("session objects bucket: %w", err)
		}
		objects = gcs
		closeFn = gcs.Close
	default:
		objects = pgstore.NewSessionObjectStore(pool)
	}

	payloads := pgstore.NewConversationPayloadStore(pool).WithObjects(objects, cfg.RawBodiesInObjects(), cfg.RawBodiesInPostgres())
	states := pgstore.NewSessionArchiveStore(pool)
	builder := sessionarchive.NewBuilder(objects, states, payloads, sessionarchive.BuilderOptions{
		MergeSegments: cfg.MergeSegments,
		Logger:        logger,
	})
	var query *sessionarchive.QueryClient
	if cfg.QueryURL != "" {
		query = sessionarchive.NewQueryClient(cfg.QueryURL, cfg.QueryToken, cfg.QueryTimeout)
	}
	return &Services{
		Config:   cfg,
		Objects:  objects,
		Payloads: payloads,
		States:   states,
		Builder:  builder,
		Reader:   sessionarchive.NewReader(objects),
		Query:    query,
		logger:   logger,
		close:    closeFn,
		baseCtx:  ctx,
	}, nil
}

// FindTranscripts returns every tenant's transcript for a conversation id.
func (s *Services) FindTranscripts(ctx context.Context, conversationID string) ([]*sessionarchive.State, error) {
	return s.States.FindByConversation(ctx, conversationID)
}

// ConversationTenants lists tenants with captures for a conversation id.
func (s *Services) ConversationTenants(ctx context.Context, conversationID string) ([]pgstore.ConversationTenant, error) {
	return s.States.ConversationTenants(ctx, conversationID)
}

// ListTurns returns one transcript generation's turns.
func (s *Services) ListTurns(ctx context.Context, transcriptID string, generation int) ([]sessionarchive.Turn, error) {
	return s.States.ListTurns(ctx, transcriptID, generation)
}

// TurnCosts returns recorded cost per request id.
func (s *Services) TurnCosts(ctx context.Context, transcriptID string, generation int) (map[string]float64, error) {
	return s.States.TurnCosts(ctx, transcriptID, generation)
}

// TurnIPs returns the distinct client IPs observed for a transcript's
// requests, most-used first.
func (s *Services) TurnIPs(ctx context.Context, transcriptID string, generation int) ([]pgstore.SessionIP, error) {
	return s.States.TurnIPs(ctx, transcriptID, generation)
}

// OwnerNames resolves tenant and user display names.
func (s *Services) OwnerNames(ctx context.Context, tenantID, userID string) (pgstore.OwnerNames, error) {
	return s.States.OwnerNames(ctx, tenantID, userID)
}

// TranscriptPage renders a page of the conversation's current transcript.
// State, turns and objects are read without a shared snapshot, so a merge or
// rebuild committing in between can delete an object this read expected; that
// surfaces as blob.ErrNotFound and the read is retried against fresh state.
func (s *Services) TranscriptPage(ctx context.Context, tenantID, conversationID string, fromSeq, limit int) (*sessionarchive.TranscriptPage, *sessionarchive.State, error) {
	return s.transcriptPage(ctx, s.States, tenantID, conversationID, fromSeq, limit)
}

type pageStateSource interface {
	Get(context.Context, string, string) (*sessionarchive.State, error)
	ListTurns(context.Context, string, int) ([]sessionarchive.Turn, error)
}

func (s *Services) transcriptPage(ctx context.Context, states pageStateSource, tenantID, conversationID string, fromSeq, limit int) (*sessionarchive.TranscriptPage, *sessionarchive.State, error) {
	for attempt := 0; ; attempt++ {
		state, err := states.Get(ctx, tenantID, conversationID)
		if err != nil {
			return nil, nil, err
		}
		if state == nil {
			if s.StartRebuild(tenantID, conversationID) {
				return nil, nil, sessionarchive.ErrTranscriptRebuilding
			}
			return nil, nil, sessionarchive.ErrTranscriptNotFound
		}
		if state.NeedsRebuild || !state.StoredIn(s.Objects.Location()) {
			s.StartRebuild(tenantID, conversationID)
			return nil, state, sessionarchive.ErrTranscriptRebuilding
		}
		if s.Query != nil && !state.NeedsRebuild {
			page, err := s.Query.Page(ctx, state, fromSeq, limit)
			if err == nil {
				return page, state, nil
			}
			if ctx.Err() != nil {
				return nil, state, ctx.Err()
			}
			s.logger.Debug().Err(err).Msg("Parquet page unavailable; reading source transcript")
		}
		turns, err := states.ListTurns(ctx, state.TranscriptID, state.Generation)
		if err != nil {
			return nil, nil, err
		}
		page, err := s.Reader.Page(ctx, state, turns, fromSeq, limit)
		if errors.Is(err, blob.ErrNotFound) && attempt < 2 {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		return page, state, nil
	}
}

// StartRebuild regenerates a transcript in the background. It returns false
// when this process is already rebuilding the conversation.
func (s *Services) StartRebuild(tenantID, conversationID string) bool {
	if s.States == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(s.baseCtx, 5*time.Second)
	defer cancel()
	queued, err := s.States.EnqueueRepair(ctx, tenantID, conversationID)
	if err != nil {
		s.logger.Error().Err(err).Msg("enqueue transcript repair failed")
		return false
	}
	return queued
}

// ObjectsLocation identifies the configured object store.
func (s *Services) ObjectsLocation() string {
	return s.Objects.Location()
}

// Rebuilding reports whether this process is rebuilding the conversation.
func (s *Services) Rebuilding(tenantID, conversationID string) bool {
	return s.Builder.Rebuilding(tenantID, conversationID)
}

// CaptureStore returns the store the capture recorder writes through: raw
// bodies always, plus capture-time transcript building when enabled.
func (s *Services) CaptureStore(metrics sessionarchive.Metrics) capture.PayloadStore {
	var builder *sessionarchive.Builder
	if s.Config.Enabled {
		builder = s.Builder
	}
	return sessionarchive.NewCaptureStore(s.Payloads, builder, s.logger, metrics)
}

// Close releases the object store client.
func (s *Services) Close() error {
	if s == nil {
		return nil
	}
	if s.repairCancel != nil {
		s.repairCancel()
		<-s.repairDone
	}
	if s.close == nil {
		return nil
	}
	return s.close()
}
