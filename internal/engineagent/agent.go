// Package engineagent is the agent half of the control channel: the thing
// that runs next to an engine on a rented box and tells the control plane
// that engine exists.
//
// It is deliberately small. Register, sleep, register again. There is no
// reconnect logic, no backoff state machine and no stream to tend, because a
// lease has none to have. An agent that crashes and restarts simply registers
// again, and the control plane reconciles by id rather than by tracking a
// session. That is a large part of why docs/design/0006 Part 4 chose a lease
// over a held-open stream.
//
// # What is told and what is discovered
//
// The split matters more than the loop does. A box cannot see its own
// provider identity from inside: hostname is a container id and no provider
// exports a machine id into the environment (docs/design/0007 finding 4). So
// the agent's span is partly told to it and partly read by it:
//
//   - told, injected at deploy time: engine id, deployment id, provider,
//     host id, node index, endpoint, and where the control plane is
//   - discovered, read on the box: how many cards are here, and later the
//     link health of issue 213
//
// Failure attribution (issue 214) is only ever as good as what the deploy
// path stamped, which is why the injection is its own reviewable surface
// rather than an implementation detail of this package.
//
// CP/DP placement: this runs on the data-plane side and reaches the control
// plane only through the generated Connect client, which is what CONSTRAINTS
// CP/DP-1 requires. It never imports internal/provisioners.
package engineagent

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Registrar is the one control-plane call an agent makes. Narrow on purpose
// so tests substitute a fake and the real implementation stays the generated
// Connect client (provisionerv1connect.EngineRegistryServiceClient satisfies
// this without an adapter).
type Registrar interface {
	RegisterEngine(context.Context, *connect.Request[provisionerv1.RegisterEngineRequest]) (*connect.Response[provisionerv1.RegisterEngineResponse], error)
}

// Identity is everything the agent is told rather than able to read. Every
// field here arrives from the control plane at deploy time.
//
// HostID is the one worth pausing on. It is the provider's machine id
// (RunPod's top-level machineId, Vast's machine_id, an EC2 instance id) and
// it is unreadable from inside the container, so an empty HostID means the
// deploy path did not stamp it and issue 214 will not be able to attribute a
// failure to a node. The agent reports the gap rather than inventing a
// substitute, because a container id that looks like a host id is worse than
// no host id.
type Identity struct {
	EngineID     string
	DeploymentID string
	Model        string
	Endpoint     string
	Provider     string
	HostID       string
	NodeIndex    int32
}

// Probe reports whether the engine on this box is serving yet. It returns a
// bool rather than an error because the agent does not act differently on
// "not ready" and "the probe itself failed": both mean the group has not
// formed, and both are ASSEMBLING.
type Probe func(context.Context) bool

// Agent registers one engine and keeps its lease alive.
type Agent struct {
	client Registrar
	ident  Identity
	probe  Probe
	cards  int32
	span   []*provisionerv1.EngineNode
	log    *slog.Logger

	// interval is the renewal cadence. Seeded with a conservative default
	// and replaced by whatever the control plane returns, so detection
	// latency stays tunable in one place rather than per agent.
	interval time.Duration
}

// DefaultInterval is used only until the first successful registration tells
// the agent what the control plane actually wants. It is deliberately
// shorter than any plausible lease so a slow first answer does not cost a
// missed renewal.
const DefaultInterval = 5 * time.Second

// Option configures an Agent.
type Option func(*Agent)

// WithProbe supplies the local readiness check that separates ASSEMBLING
// from SERVING. Without one the agent reports SERVING from the first
// registration, which is right for an engine with no health endpoint and
// wrong for everything else.
func WithProbe(p Probe) Option {
	return func(a *Agent) {
		if p != nil {
			a.probe = p
		}
	}
}

// WithCards sets the GPU count this node contributes. Callers normally pass
// the result of CountCards.
func WithCards(n int32) Option {
	return func(a *Agent) {
		if n > 0 {
			a.cards = n
		}
	}
}

// WithSpan replaces the single-node span the agent would otherwise report
// about itself.
//
// An agent knows the node it runs on and nothing more, so the default span
// is that one node. A multi-node engine's real composition comes from the
// engine, which settles ranks across workers before it serves a token, and
// one agent then reports the whole group. That path needs a cross-node
// provider primitive, which docs/design/0006 Part 3 shows no provider we can
// reach exposes today, so issue 212 is unscheduled and this seam currently
// has exactly one caller: the mock-engine harness, which fabricates a
// multi-node span so the fleet view's span column can be exercised without
// renting a pool.
//
// Naming that honestly matters. This is where the real thing will plug in,
// and until then it is a demo affordance.
func WithSpan(span []*provisionerv1.EngineNode) Option {
	return func(a *Agent) {
		if len(span) > 0 {
			a.span = span
		}
	}
}

// WithLogger overrides the logger.
func WithLogger(l *slog.Logger) Option {
	return func(a *Agent) {
		if l != nil {
			a.log = l
		}
	}
}

// WithInterval overrides the starting renewal cadence. The control plane's
// answer still wins on the first successful registration; this only affects
// how often the agent retries before it has ever been answered.
func WithInterval(d time.Duration) Option {
	return func(a *Agent) {
		if d > 0 {
			a.interval = d
		}
	}
}

// New builds an Agent. ident.EngineID is required: without it the registry
// cannot tell a renewal from a second member.
func New(client Registrar, ident Identity, opts ...Option) (*Agent, error) {
	if ident.EngineID == "" {
		return nil, fmt.Errorf("engineagent: engine id is required")
	}
	a := &Agent{
		client:   client,
		ident:    ident,
		log:      slog.Default(),
		interval: DefaultInterval,
	}
	for _, o := range opts {
		o(a)
	}
	return a, nil
}

// Run registers until ctx is cancelled.
//
// A failed registration logs and retries on the next tick. It must not be
// fatal: the control plane being briefly unreachable is not a reason for a
// serving engine to stop serving, and the lease already encodes what happens
// if the outage outlasts it. This is the same reason the binary fetch in the
// container entrypoint is non-fatal. Nothing about the fleet view is worth
// costing a request.
func (a *Agent) Run(ctx context.Context) {
	for {
		a.registerOnce(ctx)

		select {
		case <-ctx.Done():
			return
		case <-time.After(a.interval):
		}
	}
}

// registerOnce sends one registration and adopts the returned cadence.
// Separated from Run so tests drive a single tick without a clock.
func (a *Agent) registerOnce(ctx context.Context) {
	e := a.snapshot(ctx)

	resp, err := a.client.RegisterEngine(ctx, connect.NewRequest(
		&provisionerv1.RegisterEngineRequest{Engine: e}))
	if err != nil {
		a.log.Warn("engine registration failed",
			"engine", a.ident.EngineID, "err", err)
		return
	}

	// The control plane owns the cadence. Renewing at a fraction of the
	// lease leaves slack for a dropped request, and doing the division here
	// rather than shipping two numbers keeps the lease the single knob.
	if lease := resp.Msg.GetLeaseSeconds(); lease > 0 {
		a.interval = time.Duration(lease) * time.Second / RenewDivisor
	}
	a.log.Info("engine registered",
		"engine", a.ident.EngineID,
		"state", e.GetState().String(),
		"cards", a.cards,
		"lease_seconds", resp.Msg.GetLeaseSeconds())
}

// RenewDivisor is how many renewal attempts fit inside one lease. Three
// gives two chances before expiry. It mirrors the control plane's own
// constant; the agent recomputes rather than being told the interval so a
// control plane that has not been upgraded still yields a sane cadence.
const RenewDivisor = 3

// snapshot builds the registration for this tick.
//
// The state is read fresh every time rather than latched, so an engine that
// falls over after assembling reports ASSEMBLING again instead of claiming a
// readiness it no longer has. LOST is never reported here: it is the control
// plane's conclusion from an expired lease, and the registry rejects an
// engine that claims it about itself.
func (a *Agent) snapshot(ctx context.Context) *provisionerv1.Engine {
	state := provisionerv1.EngineState_ENGINE_STATE_SERVING
	if a.probe != nil && !a.probe(ctx) {
		state = provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING
	}

	span := a.span
	if span == nil {
		span = []*provisionerv1.EngineNode{{
			HostId:    a.ident.HostID,
			Provider:  a.ident.Provider,
			NodeIndex: a.ident.NodeIndex,
			GpuCount:  a.cards,
		}}
	}

	return &provisionerv1.Engine{
		Id:           a.ident.EngineID,
		DeploymentId: a.ident.DeploymentID,
		Model:        a.ident.Model,
		Endpoint:     a.ident.Endpoint,
		State:        state,
		Span:         span,
	}
}

// HTTPProbe returns a Probe that treats a 2xx from url as "serving".
//
// The agent runs on the box, so this is a loopback call to the engine's own
// health endpoint and costs nothing worth budgeting. That locality is the
// point: the control plane's /health poller cannot see ASSEMBLING because
// during assembly there is no reachable endpoint for it to ask, while the
// agent is already inside and can watch the engine come up.
func HTTPProbe(url string, timeout time.Duration) Probe {
	client := &http.Client{Timeout: timeout}
	return func(ctx context.Context) bool {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode >= 200 && resp.StatusCode < 300
	}
}
