package sessionstorage

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/earlgrey/piproxy/internal/blob"
	"github.com/earlgrey/piproxy/internal/metrics"
	"github.com/earlgrey/piproxy/internal/sessionarchive"
	pgstore "github.com/earlgrey/piproxy/internal/store/postgres"
)

// StartRepairWorker starts only in the proxy, once per process. PostgreSQL
// pending flags, retry times, and fenced leases survive restarts and coordinate
// replicas. Cloud preparation never retains a database connection.
func (s *Services) StartRepairWorker(parent context.Context, m *metrics.SessionArchiveMetrics) {
	if !s.Config.Enabled {
		return
	}
	s.repairStart.Do(func() {
		ctx, cancel := context.WithCancel(parent)
		s.repairCancel = cancel
		s.repairDone = make(chan struct{})
		go func() { defer close(s.repairDone); s.runRepairs(ctx, m) }()
	})
}

func (s *Services) runRepairs(ctx context.Context, m *metrics.SessionArchiveMetrics) {
	nextInventory := time.Time{}
	for ctx.Err() == nil {
		if !time.Now().Before(nextInventory) {
			inventoryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			refs, err := s.States.ConversationsNeedingTranscripts(inventoryCtx, pgstore.BackfillFilter{Limit: 100, UnqueuedOnly: true, ObjectsLocation: s.Objects.Location()})
			if err == nil {
				for _, ref := range refs {
					if _, err = s.States.EnqueueRepair(inventoryCtx, ref.TenantID, ref.ConversationID); err != nil {
						break
					}
				}
			}
			if err != nil && ctx.Err() == nil {
				s.logger.Warn().Msg("transcript repair reconciliation failed")
			}
			count, oldest, err := s.States.RepairBacklog(inventoryCtx)
			if err == nil {
				m.RepairBacklog(count, oldest)
			}
			cancel()
			nextInventory = time.Now().Add(30 * time.Second)
		}
		claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		lease, err := s.States.ClaimRepair(claimCtx)
		cancel()
		if err != nil && ctx.Err() == nil {
			s.logger.Warn().Msg("transcript repair claim failed")
		}
		if lease == nil || err != nil {
			timer := time.NewTimer(2 * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			continue
		}
		started := time.Now()
		jobCtx, cancel := context.WithTimeout(ctx, rebuildTimeout)
		stage := "archive"
		err = withRepairLease(jobCtx, lease.ExpiresAt, func(c context.Context) (time.Time, error) { return s.States.RenewRepair(c, lease) }, func(c context.Context) error {
			if _, err := s.Payloads.ArchiveConversation(c, lease.TenantID, lease.ConversationID); err != nil {
				return err
			}
			stage = "rebuild"
			builder := sessionarchive.NewBuilder(s.Objects, s.States.WithRepairLease(lease.Token), s.Payloads,
				sessionarchive.BuilderOptions{MergeSegments: s.Config.MergeSegments, Logger: s.logger})
			_, err := builder.Rebuild(c, lease.TenantID, lease.ConversationID)
			return err
		})
		cancel()
		reason := repairReason(err)
		finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		if finishErr := s.States.FinishRepair(finishCtx, lease, reason); finishErr != nil {
			s.logger.Warn().Msg("transcript repair claim release failed")
		}
		finishCancel()
		m.RepairFinished(reason)
		hash := sha256.Sum256([]byte(lease.TenantID + "\x00" + lease.ConversationID))
		log := s.logger.With().Str("session_hash", fmt.Sprintf("%x", hash[:8])).Str("stage", stage).Dur("elapsed", time.Since(started)).Logger()
		if err != nil {
			log.Warn().Str("reason", reason).Msg("transcript repair pending retry")
		} else {
			log.Info().Msg("transcript repair completed")
		}
	}
}

func repairReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, pgstore.ErrRepairLeaseLost):
		return "lease_lost"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	case errors.Is(err, blob.ErrNotFound):
		return "source_missing"
	case errors.Is(err, pgstore.ErrArchiveIntegrity):
		return "archive_integrity"
	default:
		return "repair_failed"
	}
}

// A blocked renewal never extends the last successful deadline. The watchdog
// remains responsive independently of the database request. Join renewal work
// before releasing the claim or closing its pool.
func withRepairLease(parent context.Context, expiry time.Time, renew func(context.Context) (time.Time, error), work func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(20 * time.Second)
		defer ticker.Stop()
		watchdog := time.NewTimer(max(time.Until(expiry)-5*time.Second, 0))
		defer watchdog.Stop()
		type renewal struct {
			expiry time.Time
			err    error
		}
		results := make(chan renewal, 1)
		pending := false
		var wg sync.WaitGroup
		defer wg.Wait()
		for {
			select {
			case <-ctx.Done():
				return
			case <-watchdog.C:
				cancel(pgstore.ErrRepairLeaseLost)
				return
			case <-ticker.C:
				if pending {
					continue
				}
				pending = true
				wg.Add(1)
				go func() {
					defer wg.Done()
					c, stop := context.WithTimeout(ctx, 5*time.Second)
					defer stop()
					e, err := renew(c)
					results <- renewal{e, err}
				}()
			case r := <-results:
				pending = false
				if errors.Is(r.err, pgstore.ErrRepairLeaseLost) {
					cancel(pgstore.ErrRepairLeaseLost)
					return
				}
				if r.err == nil {
					if !watchdog.Stop() {
						select {
						case <-watchdog.C:
						default:
						}
					}
					watchdog.Reset(max(time.Until(r.expiry)-5*time.Second, 0))
				}
			}
		}
	}()
	err := work(ctx)
	contextErr := context.Cause(ctx)
	cancel(context.Canceled)
	<-done
	if (err == nil || errors.Is(err, context.Canceled)) && contextErr != nil {
		return contextErr
	}
	return err
}
