// Package lifecycle holds long-running goroutines the daemon launches
// to maintain provisioner state-of-record beyond per-request handlers.
//
// v0.2 ch7-beat1.7 ships the first lifecycle loop: the idle-TTL
// reaper. The brief calls it "non-negotiable from day one" -- without
// it, an operator who walks away from `iplane up` (or any deployment)
// leaves a paid GPU billing all night. The reaper sweeps active
// deployments on a tick and destroys any whose idle TTL has elapsed.
//
// The reaper goes through the public Service API (ListDeployments +
// DestroyDeployment) rather than reaching into store internals --
// matches the operator-facing surface and makes the reaper
// indistinguishable from any other Service consumer. Same pattern
// the chapter teaches in act-3 of Ch 7.
package lifecycle

import (
	"context"
	"log/slog"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/metrics"
)

// DefaultInterval is the reaper's tick cadence in production. Tuned so
// idle pods get reaped within ~one interval of their TTL expiring
// without putting non-trivial load on the daemon's state store. Tests
// inject a much shorter interval via WithInterval (or call RunOnce
// directly).
const DefaultInterval = 30 * time.Second

// Service is the narrow interface the reaper needs. Declared here (not
// where the concrete *provisioners.Service lives) so the reaper depends
// only on what it uses. *provisioners.Service satisfies it directly;
// tests pass a fake.
type Service interface {
	ListDeployments(ctx context.Context, req *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error)
	DestroyDeployment(ctx context.Context, req *provisionerv1.DestroyDeploymentRequest) (*provisionerv1.DestroyDeploymentResponse, error)
}

// Reaper periodically sweeps the deployment state-of-record and
// destroys any deployments whose idle TTL has elapsed. Designed for a
// single goroutine per daemon process; RunOnce is also exposed for
// tests that want deterministic sweep control.
type Reaper struct {
	svc      Service
	clock    func() time.Time
	interval time.Duration
	recorder *metrics.Recorder
	logger   *slog.Logger
}

// Option configures the Reaper. Production wires WithRecorder + a
// real Service; tests inject WithClock for deterministic time
// progression and shorter WithInterval if exercising the Run loop.
type Option func(*Reaper)

// WithClock injects a clock function. Defaults to time.Now.
func WithClock(c func() time.Time) Option { return func(r *Reaper) { r.clock = c } }

// WithInterval sets the tick cadence. Defaults to DefaultInterval (30s).
func WithInterval(d time.Duration) Option { return func(r *Reaper) { r.interval = d } }

// WithRecorder wires metric emission. nil-safe; when unset, reap
// events still log but the iplane.reaper.destroys.total counter
// doesn't tick.
func WithRecorder(rec *metrics.Recorder) Option { return func(r *Reaper) { r.recorder = rec } }

// WithLogger sets the slog logger. Defaults to slog.Default.
func WithLogger(l *slog.Logger) Option { return func(r *Reaper) { r.logger = l } }

// New constructs a Reaper backed by the supplied Service. Pass options
// for clock / interval / recorder / logger.
func New(svc Service, opts ...Option) *Reaper {
	r := &Reaper{
		svc:      svc,
		clock:    time.Now,
		interval: DefaultInterval,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Run is the production loop. Tickers until ctx is done; calls
// RunOnce on each tick. Errors from RunOnce are logged but don't
// terminate the loop -- a transient ListDeployments failure
// shouldn't take down leak protection.
func (r *Reaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = r.RunOnce(ctx)
		}
	}
}

// RunOnce sweeps the state once and reaps any qualifying deployments.
// Returns the count of deployments destroyed in this sweep. Suitable
// for tests; production uses Run which calls this on a ticker.
func (r *Reaper) RunOnce(ctx context.Context) int {
	resp, err := r.svc.ListDeployments(ctx, &provisionerv1.ListDeploymentsRequest{})
	if err != nil {
		r.logger.Error("reaper: ListDeployments failed", "err", err)
		return 0
	}
	now := r.clock()
	var reaped int
	for _, dep := range resp.GetDeployments() {
		if !r.shouldReap(dep, now) {
			continue
		}
		_, err := r.svc.DestroyDeployment(ctx, &provisionerv1.DestroyDeploymentRequest{Id: dep.GetId()})
		if err != nil {
			r.logger.Error("reaper: destroy failed", "id", dep.GetId(), "err", err)
			continue
		}
		r.logger.Info("reaper: destroyed idle deployment",
			"id", dep.GetId(),
			"model", dep.GetModel(),
			"idle_ttl_seconds", dep.GetIdleTtlSeconds(),
			"last_activity", lastActivityFor(dep))
		r.recorder.RecordReaperDestroy(ctx, "idle")
		reaped++
	}
	return reaped
}

// shouldReap encapsulates the reap policy. Documented in plain English
// because the chapter prose teaches this exact decision tree:
//
//   - state must be RUNNING. Destroying a STARTING/CONFIGURING
//     deployment would clobber an in-progress deploy; FAILED /
//     TERMINATED / TERMINATING are already done; DEGRADED stays for
//     operator diagnostics.
//   - idle_ttl_seconds must be > 0. Zero means "no TTL set", the
//     v0.2 default that preserves v0.1 behavior (no reaping unless
//     operator opts in).
//   - no_idle_destroy must be false. The pin flag (#72) is the
//     operator's "don't touch this deployment regardless of TTL"
//     escape hatch.
//   - now - last_activity > ttl. last_activity_at falls back to
//     created_at when never touched (a fresh deployment with no
//     traffic yet); that's the right starting clock for "this thing
//     has been idle since it was born."
func (r *Reaper) shouldReap(dep *provisionerv1.Deployment, now time.Time) bool {
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		return false
	}
	if dep.GetIdleTtlSeconds() <= 0 {
		return false
	}
	if dep.GetNoIdleDestroy() {
		return false
	}
	lastAct := lastActivityFor(dep)
	if lastAct.IsZero() {
		return false
	}
	age := now.Sub(lastAct)
	return age > time.Duration(dep.GetIdleTtlSeconds())*time.Second
}

// lastActivityFor returns the deployment's effective "last activity"
// timestamp: prefers LastActivityAt when set, falls back to CreatedAt
// (a fresh deployment with no traffic is "idle since creation"),
// returns the zero Time when neither is set (defensive; should be
// impossible since CreateDeployment stamps CreatedAt unconditionally).
func lastActivityFor(dep *provisionerv1.Deployment) time.Time {
	if ts := dep.GetLastActivityAt(); ts != nil {
		return ts.AsTime()
	}
	if ts := dep.GetCreatedAt(); ts != nil {
		return ts.AsTime()
	}
	return time.Time{}
}
