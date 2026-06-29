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

// MaxDestroyRetries caps how many times the reaper will keep trying
// to destroy a deployment stuck in TERMINATING before giving up.
// Three sweeps at DefaultInterval = 90s, well past a typical transient
// RunPod API blip but short enough that a truly stuck deployment
// stops consuming reaper bandwidth.
//
// On exceeding the cap, the reaper logs a loud warning and stops
// retrying that id. The deployment remains in TERMINATING with the
// failure_reason set; operator intervention (manual destroy, RunPod
// console cleanup) is required. Issue 165 tracks adding an explicit
// FAILED transition when the cap fires.
const MaxDestroyRetries = 3

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
//
// The reaper also retries stuck destroys: deployments in TERMINATING
// (their previous DestroyDeployment hit a transient error, e.g.
// RunPod API timeout) get retried up to MaxDestroyRetries times.
// destroyRetries tracks per-id attempt counts in-memory; a daemon
// restart resets the counter, which is fine -- worst case is a few
// extra retry attempts on already-stuck pods.
type Reaper struct {
	svc             Service
	clock           func() time.Time
	interval        time.Duration
	recorder        *metrics.Recorder
	logger          *slog.Logger
	destroyRetries  map[string]int
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
		svc:            svc,
		clock:          time.Now,
		interval:       DefaultInterval,
		logger:         slog.Default(),
		destroyRetries: map[string]int{},
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
//
// Two reap paths:
//
//   - RUNNING deployments whose idle TTL has elapsed: the original
//     leak-protection sweep.
//   - TERMINATING deployments: previous destroy attempts hit a
//     transient error (issue 165). Each sweep increments an in-memory
//     retry counter; after MaxDestroyRetries the reaper gives up
//     (loud warning) and the operator has to clean up manually.
//
// Successful destroys clear any retry-counter entry for the id so
// future re-creations of the same id (unusual but possible) start
// fresh.
func (r *Reaper) RunOnce(ctx context.Context) int {
	resp, err := r.svc.ListDeployments(ctx, &provisionerv1.ListDeploymentsRequest{})
	if err != nil {
		r.logger.Error("reaper: ListDeployments failed", "err", err)
		return 0
	}
	now := r.clock()
	var reaped int
	for _, dep := range resp.GetDeployments() {
		switch {
		case r.shouldReap(dep, now):
			if r.destroy(ctx, dep, "idle") {
				reaped++
			}
		case r.shouldRetry(dep):
			if r.destroy(ctx, dep, "retry") {
				reaped++
			}
		}
	}
	return reaped
}

// destroy invokes the Service and handles bookkeeping. Returns true on
// successful TERMINATED transition, false otherwise. reason labels the
// reaper metric ("idle" for normal sweeps, "retry" for stuck-destroy
// retries).
func (r *Reaper) destroy(ctx context.Context, dep *provisionerv1.Deployment, reason string) bool {
	id := dep.GetId()
	attempt := r.destroyRetries[id] + 1
	if attempt > MaxDestroyRetries {
		// Already exceeded the cap on a prior sweep; suppress further
		// attempts to keep the log quiet and the API quiet. The id
		// stays in the retries map at the cap value as a tombstone.
		return false
	}

	_, err := r.svc.DestroyDeployment(ctx, &provisionerv1.DestroyDeploymentRequest{Id: id})
	if err != nil {
		r.destroyRetries[id] = attempt
		if attempt >= MaxDestroyRetries {
			r.logger.Error("reaper: destroy failed; giving up after retry cap",
				"id", id,
				"attempts", attempt,
				"max_retries", MaxDestroyRetries,
				"err", err,
				"action_required", "manual cleanup (iplane deployment destroy + provider console)")
		} else {
			r.logger.Warn("reaper: destroy failed; will retry",
				"id", id,
				"attempt", attempt,
				"max_retries", MaxDestroyRetries,
				"err", err)
		}
		return false
	}

	delete(r.destroyRetries, id)
	r.logger.Info("reaper: destroyed deployment",
		"id", id,
		"model", dep.GetModel(),
		"reason", reason,
		"idle_ttl_seconds", dep.GetIdleTtlSeconds(),
		"last_activity", lastActivityFor(dep))
	r.recorder.RecordReaperDestroy(ctx, reason)
	return true
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
// shouldRetry decides whether a TERMINATING deployment is a retry
// candidate. State is the only gate today; per-id attempt counting
// inside destroy() handles the cap. A deployment in TERMINATING means
// a prior destroy call started but failed before reaching TERMINATED
// (e.g. runpod adapter classified the error as transient per issue
// 165); the reaper picks it back up automatically.
//
// Excluded by design:
//   - FAILED: the deployer hit a permanent error (auth, validation).
//     Retrying won't help; operator action is required. Issue 165
//     follow-up may add a separate "force destroy" path.
//   - RUNNING: handled by shouldReap. Don't double-dispatch.
func (r *Reaper) shouldRetry(dep *provisionerv1.Deployment) bool {
	return dep.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING
}

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
