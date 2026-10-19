package memory

import (
	"context"

	"github.com/rs/zerolog"

	"github.com/earlgrey/piproxy/internal/hooks"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

type sessionDistillationEnqueuer interface {
	AllowsProject(ctx context.Context, tenantID, projectID string) (bool, error)
	EnqueueSession(ctx context.Context, identity pgstore.SessionIdentity) error
}

type tenantDistillationConsent interface {
	TenantDistillationEnabled(ctx context.Context, tenantID string) (bool, error)
}

// NewSessionUploadedHook asynchronously persists one idempotent Absurd task
// for the session. The hook manager owns the async execution; the task itself
// sleeps durably until the configured nightly run.
func NewSessionUploadedHook(queue sessionDistillationEnqueuer, consent tenantDistillationConsent, logger zerolog.Logger) hooks.Handler {
	return hooks.HandlerFunc{
		HandlerName: "project-memory-session-upload",
		EventTypes:  []hooks.EventType{hooks.EventSessionUploaded},
		Fn: func(ctx context.Context, ev *hooks.Event) (*hooks.Decision, error) {
			if queue == nil || consent == nil || ev.ConversationID == "" || ev.TenantID == "" || ev.KeyID == "" {
				return nil, nil
			}
			allowed, err := queue.AllowsProject(ctx, ev.TenantID, ev.ProjectID)
			if err != nil {
				return nil, err
			}
			if !allowed {
				logger.Debug().
					Str("conversation_id", ev.ConversationID).
					Str("project", ev.Project).
					Str("project_id", ev.ProjectID).
					Msg("session memory distillation skipped by project allowlist")
				return nil, nil
			}
			enabled, err := consent.TenantDistillationEnabled(ctx, ev.TenantID)
			if err != nil {
				return nil, err
			}
			if !enabled {
				return nil, nil
			}
			ownerID := "key:" + ev.KeyID
			if ev.UserID != "" {
				ownerID = "user:" + ev.UserID
			}
			identity := pgstore.SessionIdentity{TenantID: ev.TenantID, OwnerID: ownerID, ConversationID: ev.ConversationID}
			if err := queue.EnqueueSession(ctx, identity); err != nil {
				return nil, err
			}
			logger.Info().
				Str("conversation_id", ev.ConversationID).
				Str("project", ev.Project).
				Str("project_id", ev.ProjectID).
				Msg("session queued for nightly memory distillation")
			return nil, nil
		},
	}
}
