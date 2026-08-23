// Package enginewait holds the poll loop an image-native provider runs
// between renting a box and the engine answering /health.
//
// It exists because that loop was written twice and the two copies drifted
// in the way copies do: the terminal-failure guard that stops a dead host
// billing the whole timeout was added to one of them and never reached the
// other, which is issue #268's actual complaint rather than the line count.
//
// What is genuinely shared is the shape: tick until a deadline, ask the
// provider what it can see, probe /health, advance a monotonic phase, emit
// one progress update per tick. What is not shared is what a tick can
// observe, and that is the part each adapter keeps. RunPod knows its
// endpoint before it starts and has to ask a second API for the phase;
// Vast discovers the endpoint by polling and gets the phase off the record
// it already fetched. Those differences are real, so they stay in the
// Observe callback rather than being flattened into a common shape that
// fits neither.
//
// The sshdocker executor deliberately does not use this. It probes by
// running curl over SSH inside the container rather than dialing an
// endpoint, its caller owns the deadline, and it has no endpoint to
// discover or return. It shares the name of the problem, not the loop.
package enginewait

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// Ladder is a provider's cold-start phase vocabulary.
//
// The rungs themselves stay per-provider, because the stages a provider
// can actually distinguish are a property of what its API reports. What
// is shared is the rule: a phase never regresses. That matters beyond
// tidiness, since a phase change opens and closes a bucket in the deploy
// phase histogram, so a rung that rewound on a flaky read would record
// two short image-pulls where there was one long one.
type Ladder struct {
	// Ordinal ranks a phase. An unrecognised phase must rank below every
	// real rung, so a bad read is never mistaken for progress.
	Ordinal func(phase string) int
	// Description is the operator-facing prefix for a rung.
	Description func(phase string) string
}

// Observation is what one tick learned from the provider.
type Observation struct {
	// Endpoint is where the engine will answer, or "" while that is
	// still unknown. A provider that knows it up front returns the same
	// value every tick.
	Endpoint string
	// Phase is the rung this observation implies. The loop clamps it so
	// it can only ever climb.
	Phase string
	// Detail is the provider's own account of what is happening, when it
	// has one. It wins over the health probe's answer, because a host
	// saying "Retrying" is telling you something the probe cannot.
	Detail string
	// Fatal, when set, stops the wait immediately. This is how a
	// provider that can tell a container will never run stops billing
	// the rest of the timeout. A provider that cannot tell leaves it nil
	// and the loop keeps waiting, which is the safe direction.
	Fatal error
}

// Config is one provider's wiring of the loop.
type Config struct {
	Timeout  time.Duration
	Interval time.Duration
	// ContainerID rides on every emitted update so the control plane can
	// attribute progress to the right replica.
	ContainerID string
	// Endpoint seeds the loop for a provider that knows where the engine
	// will answer before it starts waiting. Left empty by a provider that
	// has to discover it, which is the difference that decides whether
	// the first tick costs a status read.
	Endpoint string
	Ladder   Ladder
	// Observe is called once per tick, before the health probe, and is
	// given the phase reached so far. Providers use that to stop paying
	// for status reads once the ladder has topped out.
	Observe func(ctx context.Context, phase string) Observation
	// Probe dials the endpoint. Returns whether the engine answered and
	// a short description of what came back otherwise.
	Probe func(ctx context.Context, endpoint string) (ok bool, detail string)
	Emit  func(provisioners.DeployStateUpdate)
	// Logs returns whatever the engine has printed, or "" when this
	// provider cannot say. Called only when the wait is about to fail,
	// and called BEFORE the caller tears anything down, because a
	// destroyed machine has no logs left to give (measured: the fetch
	// after teardown returns "No such container").
	//
	// Optional, because providers differ on whether they can answer.
	// Vast uploads instance logs on request; RunPod's REST API exposes
	// none at all, so it leaves this nil and the failure reads exactly as
	// it did before (#47).
	//
	// A wait that ends without the engine's own words costs the whole
	// rental and teaches nothing, which is not hypothetical: the GLM-5.2
	// run spent sixty minutes and $23 timing out at engine:init with no
	// record of what vLLM was doing.
	Logs func(ctx context.Context) string
}

// Wait polls until the engine answers, the deadline passes, the caller
// gives up, or the provider reports the container will never run.
//
// Returns the endpoint on success. On failure the endpoint is returned
// too when one was discovered, because a caller tearing down wants to
// name what it was waiting on.
//
// The caller's cancellation and the deadline are reported differently on
// purpose: one means the operator stopped waiting, the other means the
// box took too long, and an operator reading a failed deploy needs to
// know which.
func Wait(ctx context.Context, c Config) (string, error) {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Second
	}
	deadline := time.Now().Add(c.Timeout)
	started := time.Now()

	endpoint, phase, last := c.Endpoint, "", ""
	for {
		// Probe before observing whenever the endpoint is already known.
		// A provider whose endpoint is fixed at rent time should not pay
		// for a status read on a tick where the engine is already
		// answering, which is every tick of a warm redeploy.
		probed := false
		if endpoint != "" {
			ok, detail := c.Probe(ctx, endpoint)
			if ok {
				return endpoint, nil
			}
			last, probed = detail, true
		}

		obs := c.Observe(ctx, phase)
		if obs.Fatal != nil {
			return endpoint, withEngineLogs(ctx, c, obs.Fatal)
		}
		if obs.Endpoint != "" {
			endpoint = obs.Endpoint
		}
		// Clamp: the ladder only ever climbs.
		if c.Ladder.Ordinal(obs.Phase) >= c.Ladder.Ordinal(phase) {
			phase = obs.Phase
		}
		// The provider's own words beat the probe's, since a host
		// mid-pull explains the wait better than the refusal does.
		if obs.Detail != "" {
			last = obs.Detail
		}

		// When this tick is the one that discovered the endpoint, probe
		// it now rather than making the caller wait a whole interval for
		// an engine that may already be up.
		if endpoint != "" && !probed {
			ok, detail := c.Probe(ctx, endpoint)
			if ok {
				return endpoint, nil
			}
			if obs.Detail == "" {
				last = detail
			}
		}

		c.Emit(update(c, phase, last, time.Since(started), deadline))

		select {
		case <-ctx.Done():
			// Cause rather than Err, because a cancellation now carries a
			// reason worth reading. A deploy abandoned because its download
			// could never finish in time is a different event from an
			// operator pressing ctrl-c, and reporting both as "caller
			// stopped waiting" hides the one that cost money.
			//
			// Logs on this path too. It was the deadline's alone, on the
			// reasoning that a cancelled wait has an operator watching who
			// already knows why. An abort has no such operator.
			reason := "caller stopped waiting"
			if cause := context.Cause(ctx); cause != nil && !errors.Is(cause, ctx.Err()) {
				reason = cause.Error()
			}
			return endpoint, withEngineLogs(ctx, c, fmt.Errorf(
				"%s during %s; last: %s", reason, phase, describe(last)))
		case <-time.After(c.Interval):
		}
		if time.Now().After(deadline) {
			return endpoint, withEngineLogs(ctx, c, fmt.Errorf(
				"engine did not answer /health within %s, still at %s; last: %s",
				c.Timeout, phase, describe(last)))
		}
	}
}

// withEngineLogs appends whatever the engine printed to a failing wait.
//
// Only on the way out, and only on failure: a successful wait has nothing
// to explain, and fetching logs per tick would pay a provider round trip
// for output nobody reads. The cost of being wrong here is one extra
// request on a deploy that has already failed.
//
// A provider that cannot report logs leaves the error exactly as it was,
// because an empty "engine said:" section reads as an engine that printed
// nothing, which is a different and wronger claim than silence.
func withEngineLogs(ctx context.Context, c Config, err error) error {
	if c.Logs == nil {
		return err
	}
	tail := strings.TrimSpace(c.Logs(ctx))
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w\n--- engine said ---\n%s", err, tail)
}

// provisionerv1Update builds the per-tick progress update.
func update(c Config, phase, detail string, elapsed time.Duration, deadline time.Time) provisioners.DeployStateUpdate {
	msg := c.Ladder.Description(phase)
	if detail != "" {
		msg = fmt.Sprintf("%s: %s", msg, detail)
	}
	return provisioners.DeployStateUpdate{
		State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
		Phase:           phase,
		ProgressMessage: fmt.Sprintf("%s (%s elapsed)", msg, elapsed.Round(time.Second)),
		ContainerID:     c.ContainerID,
		Deadline:        deadline,
	}
}

func describe(s string) string {
	if s == "" {
		return "no response"
	}
	return s
}
