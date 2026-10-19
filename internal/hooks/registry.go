package hooks

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// SubscriptionLoader fetches the current enabled subscriptions. It is a
// function rather than a store interface so this package never imports the
// Postgres package, which already imports this one.
type SubscriptionLoader func(ctx context.Context) ([]*Subscription, error)

// Registry keeps a hot snapshot of the subscription set, refreshed on an
// interval so a hook added in the admin UI takes effect without a redeploy.
//
// A refresh failure keeps the previous snapshot rather than emptying it: losing
// Postgres for a minute must not silently disable a fail-closed guardrail.
type Registry struct {
	load     SubscriptionLoader
	interval time.Duration
	logger   zerolog.Logger

	snapshot atomic.Pointer[[]*Subscription]
	once     sync.Once
}

// NewRegistry creates a registry and loads the first snapshot synchronously, so
// the proxy never serves a request with an empty hook set it has not verified.
func NewRegistry(ctx context.Context, load SubscriptionLoader, interval time.Duration, logger zerolog.Logger) (*Registry, error) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	r := &Registry{load: load, interval: interval, logger: logger}
	empty := []*Subscription{}
	r.snapshot.Store(&empty)
	if err := r.refresh(ctx); err != nil {
		return nil, err
	}
	return r, nil
}

// Subscriptions implements SubscriptionSource.
func (r *Registry) Subscriptions() []*Subscription {
	if r == nil {
		return nil
	}
	return *r.snapshot.Load()
}

// Start begins periodic refresh. It is safe to call once; later calls are ignored.
func (r *Registry) Start(ctx context.Context, wg *sync.WaitGroup) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(r.interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if err := r.refresh(ctx); err != nil {
						r.logger.Warn().Err(err).Msg("hook subscription refresh failed; keeping previous set")
					}
				}
			}
		}()
	})
}

func (r *Registry) refresh(ctx context.Context) error {
	loadCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	subs, err := r.load(loadCtx)
	if err != nil {
		return err
	}
	if subs == nil {
		subs = []*Subscription{}
	}
	r.snapshot.Store(&subs)
	return nil
}

// AsyncDeliveryRecorder writes delivery records off the request path. A hook
// that already costs a turn latency must not also make it wait on an INSERT,
// and a full buffer drops records rather than blocking — the delivery log is
// diagnostics, not an audit trail.
type AsyncDeliveryRecorder struct {
	records chan DeliveryRecord
	persist func(ctx context.Context, rec DeliveryRecord) error
	logger  zerolog.Logger

	stopped   atomic.Bool
	sendMu    sync.RWMutex
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewAsyncDeliveryRecorder starts the writer goroutine.
func NewAsyncDeliveryRecorder(persist func(ctx context.Context, rec DeliveryRecord) error, bufferSize int, logger zerolog.Logger) *AsyncDeliveryRecorder {
	if bufferSize <= 0 {
		bufferSize = 512
	}
	r := &AsyncDeliveryRecorder{
		records: make(chan DeliveryRecord, bufferSize),
		persist: persist,
		logger:  logger,
	}
	r.wg.Add(1)
	go r.run()
	return r
}

// RecordDelivery implements DeliveryRecorder.
func (r *AsyncDeliveryRecorder) RecordDelivery(rec DeliveryRecord) {
	if r == nil || r.stopped.Load() {
		return
	}
	r.sendMu.RLock()
	defer r.sendMu.RUnlock()
	if r.stopped.Load() {
		return
	}
	select {
	case r.records <- rec:
	default:
		r.logger.Warn().Str("subscription_id", rec.SubscriptionID).Msg("hook delivery record dropped: buffer full")
	}
}

// Close drains the buffer and stops the writer.
func (r *AsyncDeliveryRecorder) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		r.sendMu.Lock()
		defer r.sendMu.Unlock()
		r.stopped.Store(true)
		close(r.records)
	})
	r.wg.Wait()
}

func (r *AsyncDeliveryRecorder) run() {
	defer r.wg.Done()
	for rec := range r.records {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := r.persist(ctx, rec); err != nil {
			r.logger.Warn().Err(err).Str("subscription_id", rec.SubscriptionID).Msg("hook delivery record write failed")
		}
		cancel()
	}
}
