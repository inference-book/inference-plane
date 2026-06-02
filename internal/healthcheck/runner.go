// Package healthcheck runs a single goroutine inside iplane serve
// that probes each replica's /health endpoint on a tick and calls
// Quarantine/Restore on the deployment service when a replica
// crosses the failure or success threshold.
//
// Architecture (v0.2 ch7-beat3.5, #87):
//
//   - One Runner goroutine, started by iplane serve, lives for the
//     daemon's lifetime. Sleeps between ticks.
//   - On each tick: Source.Snapshot() returns the running deployments
//     and their replicas; Runner fans out concurrent HTTP probes
//     bounded by a semaphore (MaxConcurrent). Each probe shares a
//     per-probe deadline derived from ProbeTimeout.
//   - Streak counters (per (deploy, instance)) live in a sync.Map
//     and persist across ticks. K consecutive failures -> Quarantiner.
//     Quarantine; K consecutive successes on a quarantined replica
//     -> Quarantiner.Restore.
//
// Why one runner (not one-per-deployment): a per-deployment poller
// would need lifecycle plumbing tied to deployment create/destroy
// (subscribe-and-spawn), and would still share a global streak map.
// The bounded fan-out gives 90% of the isolation (a hung probe
// doesn't starve a separate deployment) without the lifecycle
// complexity. Sharding becomes interesting when one iplane serve
// can't host the daemon's load -- v1.0 territory, not v0.2.
//
// CP/DP placement: this is control-plane code. It mutates
// Deployment state via the in-process Service (like the reaper),
// imports internal/provisioners directly, and does not touch the
// request hot path.
package healthcheck

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultConfig returns the chapter-default tuning: a 10s tick with
// a 2s probe timeout and K=3 in each direction. With these defaults
// a hung replica is quarantined within ~30s of going down -- the
// acceptance criterion called out in #87 for demo 06's narrative.
func DefaultConfig() Config {
	return Config{
		PollInterval:     10 * time.Second,
		FailureThreshold: 3,
		SuccessThreshold: 3,
		ProbeTimeout:     2 * time.Second,
		MaxConcurrent:    32,
	}
}

// Config is the runner's tuning surface. Fields with zero values
// fall back to DefaultConfig at New() time.
type Config struct {
	// PollInterval is the duration between successive ticks. Each
	// tick fans out probes against every replica's /health.
	PollInterval time.Duration
	// FailureThreshold is K_fail: the number of consecutive failed
	// probes that triggers Quarantine on a replica not already in
	// the unhealthy set.
	FailureThreshold int
	// SuccessThreshold is K_pass: the number of consecutive passing
	// probes that triggers Restore on a currently-quarantined
	// replica.
	SuccessThreshold int
	// ProbeTimeout caps how long one /health probe may take. Should
	// be substantially shorter than PollInterval so a single hung
	// engine cannot stall later ticks.
	ProbeTimeout time.Duration
	// MaxConcurrent bounds in-flight probes inside a tick. Caps the
	// goroutine fan-out under "many deployments, many replicas."
	MaxConcurrent int
}

// DeploymentSource snapshots running deployments and their replicas
// for the runner to probe. Decoupled from the Service so tests can
// inject a fake source without standing up the full deployment
// service.
type DeploymentSource interface {
	Snapshot() []DeploymentSnapshot
}

// Quarantiner is the mutation surface the runner calls when a
// replica crosses a threshold. Both methods must be idempotent --
// the runner does not track "already requested" state across ticks,
// and a snapshot can race against a recent Quarantine/Restore.
type Quarantiner interface {
	Quarantine(deployID, instanceID string) error
	Restore(deployID, instanceID string) error
}

// DeploymentSnapshot is one running deployment plus its current
// replica set. Snapshot() returns []DeploymentSnapshot for the
// runner to probe.
type DeploymentSnapshot struct {
	DeployID string
	Replicas []ReplicaSnapshot
}

// ReplicaSnapshot is one (instance_id, engine_endpoint) pair plus
// whether the source observed this replica as currently in the
// unhealthy_instance_ids set. The Quarantined flag drives the
// shouldQuarantine / shouldRestore logic inside probe().
type ReplicaSnapshot struct {
	InstanceID  string
	Endpoint    string
	Quarantined bool
}

// Runner owns the streak state and the per-tick HTTP client. One
// Runner per iplane serve; Run blocks until the passed context is
// cancelled.
type Runner struct {
	cfg    Config
	src    DeploymentSource
	q      Quarantiner
	client *http.Client
	logger *slog.Logger

	// streaks is keyed by deployID + "/" + instanceID. Values are
	// *streak. Entries grow forever in the current implementation
	// (a destroyed deployment's keys linger); follow-up cleanup is
	// tracked separately. For demo-scale workloads this is bounded
	// and negligible.
	streaks sync.Map
}

// streak tracks per-replica consecutive-pass / consecutive-fail
// counts. Mutated under streak.mu inside probe(). One streak per
// (deploy, instance) pair.
type streak struct {
	mu     sync.Mutex
	fails  int
	passes int
}

// New constructs a Runner with the supplied config (zero fields
// fall back to defaults), source, and quarantine sink. logger may
// be nil; falls back to slog.Default().
func New(cfg Config, src DeploymentSource, q Quarantiner, logger *slog.Logger) *Runner {
	def := DefaultConfig()
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = def.PollInterval
	}
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = def.FailureThreshold
	}
	if cfg.SuccessThreshold <= 0 {
		cfg.SuccessThreshold = def.SuccessThreshold
	}
	if cfg.ProbeTimeout <= 0 {
		cfg.ProbeTimeout = def.ProbeTimeout
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = def.MaxConcurrent
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{
		cfg:    cfg,
		src:    src,
		q:      q,
		client: &http.Client{Timeout: cfg.ProbeTimeout},
		logger: logger,
	}
}

// Run blocks on the supplied context, ticking at PollInterval and
// firing probe fan-outs. Returns when the context is cancelled
// (graceful shutdown).
//
// Each tick is awaited to completion before the next tick can
// start; a tick that overruns PollInterval delays the next tick.
// In practice ProbeTimeout (default 2s) << PollInterval (default
// 10s) so overruns are rare.
func (r *Runner) Run(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.tick(ctx)
		}
	}
}

// tick takes one snapshot of the deployment list and fans out
// probes against every non-empty engine endpoint, bounded by the
// per-tick semaphore. Exported only for tests that drive the
// runner deterministically without sleeping.
func (r *Runner) tick(ctx context.Context) {
	snapshots := r.src.Snapshot()
	sem := make(chan struct{}, r.cfg.MaxConcurrent)
	var wg sync.WaitGroup
	for _, dep := range snapshots {
		for _, rep := range dep.Replicas {
			if rep.Endpoint == "" {
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func(deployID, instanceID, endpoint string, quarantined bool) {
				defer wg.Done()
				defer func() { <-sem }()
				r.probe(ctx, deployID, instanceID, endpoint, quarantined)
			}(dep.DeployID, rep.InstanceID, rep.Endpoint, rep.Quarantined)
		}
	}
	wg.Wait()
}

// probe runs one /health GET against the replica's endpoint,
// updates the streak counter, and calls Quarantine / Restore if
// the appropriate threshold was crossed.
//
// Health criteria: HTTP request succeeds, status code < 500. 4xx
// codes are considered "engine responding" -- the engine being
// up but returning 4xx (e.g., method not allowed on /health) is
// not a quarantine-worthy condition. Only network errors and 5xx
// count as failure.
func (r *Runner) probe(ctx context.Context, deployID, instanceID, endpoint string, quarantined bool) {
	healthy := r.doProbe(ctx, endpoint)

	key := deployID + "/" + instanceID
	streakAny, _ := r.streaks.LoadOrStore(key, &streak{})
	s := streakAny.(*streak)
	s.mu.Lock()
	if healthy {
		s.fails = 0
		s.passes++
	} else {
		s.passes = 0
		s.fails++
	}
	fails := s.fails
	passes := s.passes
	s.mu.Unlock()

	switch {
	case !quarantined && fails >= r.cfg.FailureThreshold:
		if err := r.q.Quarantine(deployID, instanceID); err != nil {
			r.logger.Warn("quarantine call failed",
				"deploy", deployID, "instance", instanceID, "err", err)
			return
		}
		r.logger.Info("replica quarantined",
			"deploy", deployID, "instance", instanceID,
			"consecutive_failures", fails)
	case quarantined && passes >= r.cfg.SuccessThreshold:
		if err := r.q.Restore(deployID, instanceID); err != nil {
			r.logger.Warn("restore call failed",
				"deploy", deployID, "instance", instanceID, "err", err)
			return
		}
		r.logger.Info("replica restored",
			"deploy", deployID, "instance", instanceID,
			"consecutive_successes", passes)
	}
}

// doProbe is the network call carved out so tests can stub it via
// a custom http.Client transport. Returns true iff the request
// succeeded and the status code is < 500.
func (r *Runner) doProbe(ctx context.Context, endpoint string) bool {
	probeCtx, cancel := context.WithTimeout(ctx, r.cfg.ProbeTimeout)
	defer cancel()
	url := strings.TrimRight(endpoint, "/") + "/health"
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode < 500
}

// streakKey returns the sync.Map key used to track one replica's
// pass/fail history. Exposed for tests that want to inspect state.
func streakKey(deployID, instanceID string) string {
	return fmt.Sprintf("%s/%s", deployID, instanceID)
}
