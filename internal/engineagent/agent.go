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

// Readiness is what a local observation of the engine can conclude. Three
// values, and the third is the one the chapter exists for.
//
// Deliberately narrower than EngineState. LOST and DRAINING are the control
// plane's conclusions, one from an expired lease and one from an operator
// decision, and neither is something an engine can observe about itself. A
// probe that returned EngineState could express them; this cannot, so the
// rule is enforced by the type instead of by a check that has to be
// remembered.
type Readiness int

const (
	// NotReady means the group has not formed. The engine may be starting,
	// the workers may still be finding each other, or the probe itself may
	// have failed; the agent does not distinguish, because all three mean
	// the same thing to a reader of the fleet view and none of them is
	// serving.
	NotReady Readiness = iota

	// Ready means the engine is answering.
	Ready

	// Degraded means the engine is assembled and answering correctly while
	// running well below what the hardware should give, because something
	// inside the group is impaired. It is not a failure and it is not
	// health; a probe built to separate those two has no way to say it.
	//
	// A pool with no sensor for this reports Ready, never Degraded. Absence
	// of a reading is not evidence of a problem, and reporting "degraded"
	// for every machine we cannot measure would make the state useless.
	Degraded
)

// String renders a Readiness for logs.
func (r Readiness) String() string {
	switch r {
	case Ready:
		return "ready"
	case Degraded:
		return "degraded"
	default:
		return "not-ready"
	}
}

// Probe is a local observation of the engine on this box.
//
// It returns a Readiness rather than an error because the agent does not act
// differently on "not ready" and "the probe itself failed": both mean the
// group has not formed.
//
// The reason this is a local call matters more than the reason it is pushed.
// During assembly there is no reachable endpoint for anything outside the
// machine to ask, so an observer positioned elsewhere cannot distinguish a
// group that is still forming from one that is slow, broken or gone. An
// observer inside can watch it come up. Issue 213's link sensor is the same
// argument applied to a different reading, and plugs in here.
type Probe func(context.Context) Readiness

// Agent registers one engine and keeps its lease alive.
type Agent struct {
	client Registrar
	ident  Identity
	probe  Probe
	cards  int32
	span   []*provisionerv1.EngineNode
	log    *slog.Logger

	// interconnect reads link health for this node. Separate from probe
	// because it answers a different question: probe says whether the engine
	// is serving, this says whether it is serving at full speed. Nil means
	// the agent makes no interconnect claim at all, which is what an agent
	// built before this existed did.
	interconnect func(context.Context) *provisionerv1.InterconnectHealth

	// staging reads how fast weights are landing on this node. Nil means the
	// agent makes no staging claim, which is right for an engine handed a
	// warm volume: it stages nothing, and a rate of zero would read as a
	// stall rather than as an absence.
	staging func() *provisionerv1.StagingProgress

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

// WithInterconnect supplies the link-health sensor whose reading separates a
// group serving at full speed from one serving correctly at a fraction of it.
//
// Injectable rather than always shelling out, for three reasons: tests and the
// mock engine can report a board that does not exist, an operator on a
// provider that hides the NVIDIA tooling inside the container can leave it off
// instead of paying a failing exec every renewal, and a future DCGM-based
// reader replaces it without touching the agent.
//
// Left unset, the agent makes no interconnect claim at all, which is exactly
// what an agent built before this existed did.
func WithInterconnect(read func(context.Context) *provisionerv1.InterconnectHealth) Option {
	return func(a *Agent) {
		if read != nil {
			a.interconnect = read
		}
	}
}

// WithStaging supplies the sensor that reports weights arriving on this node.
//
// Injectable for the same reasons WithInterconnect is. A test needs to drive a
// download that is not happening, the mock engine has no cache to watch, and
// an engine mounting a pre-staged volume should say nothing here rather than
// reporting that nothing is arriving.
//
// Left unset, the agent makes no staging claim at all.
func WithStaging(read func() *provisionerv1.StagingProgress) Option {
	return func(a *Agent) {
		if read != nil {
			a.staging = read
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
// readiness it no longer has, and one whose link recovers stops reporting
// degraded without anything having to clear a flag.
//
// LOST and DRAINING are never reported here. Readiness cannot express them,
// which is the point of it being narrower than EngineState.
func (a *Agent) snapshot(ctx context.Context) *provisionerv1.Engine {
	// No probe means no local observation to make, which is the honest
	// default for an engine exposing no health endpoint.
	readiness := Ready
	if a.probe != nil {
		readiness = a.probe(ctx)
	}

	var state provisionerv1.EngineState
	switch readiness {
	case Ready:
		state = provisionerv1.EngineState_ENGINE_STATE_SERVING
	case Degraded:
		state = provisionerv1.EngineState_ENGINE_STATE_SERVING_DEGRADED
	default:
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

	// Stamp the link reading onto the node this agent actually runs on, and
	// only that one. An agent can see its own board and nothing else, so
	// attributing a reading to a fabricated multi-node span would be
	// reporting hardware it never looked at. Read fresh each tick like the
	// state above, so a link that recovers stops being reported without
	// anything having to clear a flag.
	if a.interconnect != nil {
		for _, n := range span {
			if n.GetNodeIndex() == a.ident.NodeIndex {
				n.Interconnect = a.interconnect(ctx)
			}
		}
	}

	// Read staging fresh each tick, and read it whatever the state says.
	// Gating it on ASSEMBLING would be assuming the two agree: an engine can
	// answer /health while a second model is still arriving, and an engine
	// can be ASSEMBLING for reasons that have nothing to do with the disk.
	// The reading describes the disk, so let it describe the disk.
	var staging *provisionerv1.StagingProgress
	if a.staging != nil {
		staging = a.staging()
	}

	return &provisionerv1.Engine{
		Id:           a.ident.EngineID,
		DeploymentId: a.ident.DeploymentID,
		Model:        a.ident.Model,
		Endpoint:     a.ident.Endpoint,
		State:        state,
		Span:         span,
		Staging:      staging,
	}
}

// HTTPProbe returns a Probe that treats a 2xx from url as Ready.
//
// The agent runs on the box, so this is a loopback call to the engine's own
// health endpoint and costs nothing worth budgeting. That locality is the
// point: the control plane's /health poller cannot see ASSEMBLING because
// during assembly there is no reachable endpoint for it to ask, while the
// agent is already inside and can watch the engine come up.
//
// It never returns Degraded, and cannot. Health endpoints answer the
// question they were built for, which is whether the engine is serving, and
// a degraded group is serving. Detecting that a group is impaired needs a
// different reading from a different source (issue 213's link sensor);
// compose the two with AnyDegraded rather than teaching this one to guess.
func HTTPProbe(url string, timeout time.Duration) Probe {
	client := &http.Client{Timeout: timeout}
	return func(ctx context.Context) Readiness {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return NotReady
		}
		resp, err := client.Do(req)
		if err != nil {
			return NotReady
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return Ready
		}
		return NotReady
	}
}

// AnyDegraded composes a readiness probe with an impairment probe, which is
// how a health check and a link sensor combine into one reading.
//
// Not ready wins over degraded: a group that has not formed is not a
// degraded group, it is a group that is not serving, and reporting the
// milder state during startup would make assembly look like a fault. Only
// once the engine answers does an impairment reading get to downgrade it.
//
// impaired is consulted only when serving, so a sensor that is expensive or
// unavailable during startup is never called then.
func AnyDegraded(ready Probe, impaired func(context.Context) bool) Probe {
	return func(ctx context.Context) Readiness {
		r := Ready
		if ready != nil {
			r = ready(ctx)
		}
		if r != Ready || impaired == nil {
			return r
		}
		if impaired(ctx) {
			return Degraded
		}
		return Ready
	}
}
