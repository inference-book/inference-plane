package engines

import (
	"context"
	"log/slog"
	"time"
)

// DefaultSweepInterval is how often the sweeper looks for expired leases.
//
// A third of the lease, matching the renewal cadence, so an engine is
// declared LOST within roughly lease + one sweep of actually going quiet.
// Sweeping faster would not detect anything sooner, because the lease is what
// defines "gone"; it would only add state-file churn.
const DefaultSweepInterval = DefaultLease / RenewDivisor

// Sweeper expires overdue leases on a tick.
//
// One goroutine for the whole daemon, matching internal/healthcheck's Runner
// and the lifecycle reaper. A per-engine timer would mean lifecycle plumbing
// tied to registration and teardown for no gain: expiry is a cheap scan and
// the fleet is small enough that a single pass costs nothing.
type Sweeper struct {
	registry *Registry
	interval time.Duration
	log      *slog.Logger
}

// SweeperOption configures a Sweeper.
type SweeperOption func(*Sweeper)

// WithSweepInterval overrides DefaultSweepInterval.
func WithSweepInterval(d time.Duration) SweeperOption {
	return func(s *Sweeper) {
		if d > 0 {
			s.interval = d
		}
	}
}

// WithLogger attaches a logger. Defaults to slog.Default.
func WithLogger(l *slog.Logger) SweeperOption {
	return func(s *Sweeper) {
		if l != nil {
			s.log = l
		}
	}
}

// NewSweeper returns a Sweeper over registry.
func NewSweeper(registry *Registry, opts ...SweeperOption) *Sweeper {
	s := &Sweeper{registry: registry, interval: DefaultSweepInterval, log: slog.Default()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Run sweeps until ctx is cancelled. Blocking; callers run it in a goroutine.
//
// A failing sweep logs and continues rather than returning. The registry is
// observational, so a transient store error should degrade the fleet view for
// one tick rather than stop expiry for the daemon's lifetime.
func (s *Sweeper) Run(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			n, err := s.registry.ExpireOverdue()
			if err != nil {
				s.log.Warn("engine lease sweep failed", "err", err)
				continue
			}
			if n > 0 {
				s.log.Info("engines declared lost", "count", n)
			}
		}
	}
}
