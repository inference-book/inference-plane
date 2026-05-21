package sshdocker

import (
	"context"
	"errors"
	"fmt"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// StateUpdate is what the Executor emits as it progresses through the
// deployment state machine. The Service consumes these to patch the
// state file + emit WatchDeployment events.
type StateUpdate struct {
	State           provisionerv1.DeploymentState
	Phase           string // free-form: "ssh:connecting", "docker:pulling", "engine:waiting", "engine:serving"
	ProgressMessage string // free-form: human-readable status
	ContainerID     string // filled when State >= STARTING
	EngineEndpoint  string // filled when State == RUNNING
	FailureReason   string // filled when State == FAILED
}

// DialFunc opens a RemoteRunner against the given instance. Tests
// substitute a stub that returns a fake RemoteRunner; production
// uses the SSH-backed dial below.
type DialFunc func(ctx context.Context, instance *provisionerv1.Instance, key *sshkeys.KeyPair) (RemoteRunner, error)

// SSHDial is the production DialFunc.
func SSHDial(ctx context.Context, instance *provisionerv1.Instance, key *sshkeys.KeyPair) (RemoteRunner, error) {
	ssh := instance.GetSsh()
	if ssh == nil || ssh.GetHost() == "" {
		return nil, fmt.Errorf("instance %q has no SSH endpoint (deployment requires an SSH-reachable instance)", instance.GetId())
	}
	user := ssh.GetUser()
	if user == "" {
		user = "root"
	}
	port := int(ssh.GetPort())
	if port == 0 {
		port = 22
	}
	return NewSSHRunner(ctx, SSHConfig{
		Host:       ssh.GetHost(),
		Port:       port,
		User:       user,
		PrivateKey: key.Private,
	})
}

// Executor runs the deployment state machine for one Deployment at
// a time. Constructed once per Service; safe to share across
// concurrent Deploy / Destroy calls (no internal mutable state).
//
// Issue 25 tracks the future Runner abstraction that schedules
// Executor work; today the Service calls Deploy / Destroy inline.
type Executor struct {
	dial        DialFunc
	healthEvery time.Duration
	healthMax   time.Duration
}

// Option configures an Executor at construction time.
type Option func(*Executor)

// WithDial overrides the default SSHDial. Tests inject a stub that
// returns a fakeRunner so the executor is exercised end-to-end
// without a real SSH server.
func WithDial(d DialFunc) Option {
	return func(e *Executor) { e.dial = d }
}

// WithHealthPoll configures the health-check loop. every is the
// polling interval; max is the total time the executor will wait
// before declaring the deployment FAILED with a "health never
// reached 2xx" reason. Defaults: every=2s, max=2min.
//
// 2 minutes covers vLLM cold starts (HF download + weights into VRAM,
// typically 30-90s on a fresh A5000-class pod). Operators with
// slower-loading engines override via this option.
func WithHealthPoll(every, max time.Duration) Option {
	return func(e *Executor) {
		e.healthEvery = every
		e.healthMax = max
	}
}

// NewExecutor constructs an Executor with sensible defaults.
func NewExecutor(opts ...Option) *Executor {
	e := &Executor{
		dial:        SSHDial,
		healthEvery: 2 * time.Second,
		healthMax:   2 * time.Minute,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Deploy runs the deployment state machine for one deployment. emit
// receives a StateUpdate at every state transition + on
// significant progress moments (e.g., starting the pull, container
// running, health-poll round). The caller is responsible for
// patching the state file from these updates.
//
// emit MUST be non-nil and SHOULD be non-blocking (or buffered).
// Executor blocks on emit; a stuck consumer stalls the executor.
//
// Returns nil on RUNNING; returns the wrapping error on FAILED. In
// either case, emit has already received the terminal StateUpdate
// before Deploy returns.
func (e *Executor) Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(StateUpdate)) error {
	if emit == nil {
		return errors.New("Deploy: emit callback is required")
	}
	if key == nil {
		return e.failed(emit, "ssh:no-key", "no ssh key provided", errors.New("deploy requires an ssh key from the Service's key store"))
	}
	if dep == nil || inst == nil {
		return errors.New("Deploy: deployment and instance are required")
	}

	emit(StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING, Phase: "ssh:connecting", ProgressMessage: fmt.Sprintf("ssh %s@%s:%d", inst.GetSsh().GetUser(), inst.GetSsh().GetHost(), inst.GetSsh().GetPort())})

	runner, err := e.dial(ctx, inst, key)
	if err != nil {
		return e.failed(emit, "ssh:connecting", "ssh connect failed", err)
	}
	defer runner.Close()

	d := NewDocker(runner)
	name := ContainerName(dep.GetId())

	// Step 1: inspect what's currently on the box.
	state, err := d.Inspect(ctx, name)
	if err != nil {
		return e.failed(emit, "docker:inspect", "inspect failed", err)
	}

	// Step 2: drift decision.
	if state.Matches(dep.GetImage(), dep.GetModel()) {
		// Container already running the desired image+model -- no-op.
		// Skip directly to health verification (still want to confirm
		// the engine is actually serving before declaring RUNNING).
		emit(StateUpdate{
			State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
			Phase:           "engine:waiting",
			ProgressMessage: "container matches desired state; verifying health",
			ContainerID:     state.ContainerID,
		})
	} else {
		if state.Exists {
			// Drift: stop + remove the stale container, then run a fresh one.
			emit(StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING, Phase: "docker:stopping", ProgressMessage: "stopping drifted container", ContainerID: state.ContainerID})
			if err := d.Stop(ctx, name); err != nil {
				return e.failed(emit, "docker:stopping", "stop failed", err)
			}
			if err := d.Remove(ctx, name); err != nil {
				return e.failed(emit, "docker:removing", "remove failed", err)
			}
		}

		// docker pull (image may already be cached; pull is fast in
		// that case but always safe to issue).
		emit(StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING, Phase: "docker:pulling", ProgressMessage: fmt.Sprintf("pulling %s", dep.GetImage())})
		if err := d.Pull(ctx, dep.GetImage()); err != nil {
			return e.failed(emit, "docker:pulling", "pull failed", err)
		}

		// docker run.
		emit(StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING, Phase: "docker:running", ProgressMessage: fmt.Sprintf("starting %s", name)})
		containerID, err := d.Run(ctx, RunSpec{
			Name:       name,
			Image:      dep.GetImage(),
			Model:      dep.GetModel(),
			EngineArgs: dep.GetEngineArgs(),
			Env:        dep.GetEnv(),
			Port:       dep.GetEnginePort(),
		})
		if err != nil {
			return e.failed(emit, "docker:running", "docker run failed", err)
		}
		emit(StateUpdate{
			State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
			Phase:           "engine:waiting",
			ProgressMessage: "container started; waiting for engine /health",
			ContainerID:     containerID,
		})
	}

	// Step 3: poll /health until 2xx or timeout.
	port := dep.GetEnginePort()
	deadline := time.Now().Add(e.healthMax)
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ready, lastErr := e.pollHealth(pollCtx, d, port, emit)
	if !ready {
		reason := "engine /health did not return 2xx before timeout"
		if lastErr != nil {
			reason = fmt.Sprintf("%s: %v", reason, lastErr)
		}
		return e.failed(emit, "engine:waiting", reason, errors.New(reason))
	}

	// Step 4: RUNNING.
	endpoint := ""
	if inst.GetSsh().GetHost() != "" {
		// Best-effort endpoint URL: pod's SSH host on the engine port.
		// Useful only if the operator's network can reach the pod IP;
		// Phase 3's tunnel work makes this reliable. For v0.1 it's
		// surface info, not a guarantee.
		ep := port
		if ep == 0 {
			ep = 8000
		}
		endpoint = fmt.Sprintf("http://%s:%d", inst.GetSsh().GetHost(), ep)
	}
	emit(StateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		Phase:           "engine:serving",
		ProgressMessage: "engine /health is 2xx",
		EngineEndpoint:  endpoint,
	})
	return nil
}

// Destroy stops + removes the deployment's container. Idempotent:
// if the container is already gone, this is a no-op success. emit
// receives TERMINATING -> TERMINATED transitions; on error, FAILED.
func (e *Executor) Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(StateUpdate)) error {
	if emit == nil {
		return errors.New("Destroy: emit callback is required")
	}
	if key == nil {
		return e.failed(emit, "ssh:no-key", "no ssh key provided", errors.New("destroy requires an ssh key from the Service's key store"))
	}
	if dep == nil || inst == nil {
		return errors.New("Destroy: deployment and instance are required")
	}

	emit(StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING, Phase: "ssh:connecting", ProgressMessage: "connecting to instance to terminate container"})

	runner, err := e.dial(ctx, inst, key)
	if err != nil {
		return e.failed(emit, "ssh:connecting", "ssh connect failed", err)
	}
	defer runner.Close()

	d := NewDocker(runner)
	name := ContainerName(dep.GetId())

	emit(StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING, Phase: "docker:stopping", ProgressMessage: fmt.Sprintf("stopping %s", name)})
	if err := d.Stop(ctx, name); err != nil {
		return e.failed(emit, "docker:stopping", "stop failed", err)
	}
	emit(StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING, Phase: "docker:removing", ProgressMessage: fmt.Sprintf("removing %s", name)})
	if err := d.Remove(ctx, name); err != nil {
		return e.failed(emit, "docker:removing", "remove failed", err)
	}

	emit(StateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED, Phase: "done", ProgressMessage: "container stopped and removed"})
	return nil
}

// pollHealth polls /health on a fixed cadence until 2xx, ctx
// deadline, or ctx cancellation. Emits a CONFIGURING update per
// poll round so operators can see progress; the actual state stays
// CONFIGURING until the caller emits RUNNING after a successful
// pollHealth.
func (e *Executor) pollHealth(ctx context.Context, d *Docker, port int32, emit func(StateUpdate)) (bool, error) {
	ticker := time.NewTicker(e.healthEvery)
	defer ticker.Stop()
	// First check fires immediately.
	for first := true; ; first = false {
		if !first {
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-ticker.C:
			}
		}
		ok, err := d.Health(ctx, port)
		if ok {
			return true, nil
		}
		// Stay in CONFIGURING; emit a progress hint with the most
		// recent health error so the watcher sees forward motion.
		msg := "polling /health (not 2xx yet)"
		if err != nil {
			msg = fmt.Sprintf("polling /health: %v", err)
		}
		emit(StateUpdate{
			State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
			Phase:           "engine:waiting",
			ProgressMessage: msg,
		})
	}
}

// failed is the shared FAILED-state emitter. Always returns the
// wrapped error so callers can `return e.failed(...)`.
func (e *Executor) failed(emit func(StateUpdate), phase, msg string, err error) error {
	full := fmt.Sprintf("%s: %v", msg, err)
	emit(StateUpdate{
		State:         provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
		Phase:         phase,
		FailureReason: full,
	})
	return fmt.Errorf("%s: %w", phase, err)
}
