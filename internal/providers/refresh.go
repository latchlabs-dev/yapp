package providers

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Refresher periodically reloads provider configuration.
type Refresher struct {
	loader   *ConfigLoader
	interval time.Duration
	logger   zerolog.Logger
	stopCh   chan struct{}
	wg       sync.WaitGroup
}

// NewRefresher creates a new provider config refresher.
func NewRefresher(loader *ConfigLoader, interval time.Duration, logger zerolog.Logger) *Refresher {
	return &Refresher{
		loader:   loader,
		interval: interval,
		logger:   logger,
		stopCh:   make(chan struct{}),
	}
}

// Start begins the periodic refresh loop.
func (r *Refresher) Start(ctx context.Context) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()

		// Load immediately.
		if err := r.loader.LoadFromDB(ctx); err != nil {
			r.logger.Error().Err(err).Msg("initial provider config load failed")
		}

		for {
			select {
			case <-ticker.C:
				if err := r.loader.LoadFromDB(ctx); err != nil {
					r.logger.Error().Err(err).Msg("provider config refresh failed")
				}
			case <-r.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop stops the refresh loop.
func (r *Refresher) Stop() {
	close(r.stopCh)
	r.wg.Wait()
}
