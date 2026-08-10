// Package engines is the control plane's record of what data planes exist,
// learned from what announced itself rather than from what the control plane
// remembers renting.
//
// The shape is a lease, not a held-open stream. An engine registers, then
// keeps re-registering before its lease runs out; silence past the expiry is
// how the control plane learns it is gone. docs/design/0006 Part 4 argues the
// choice in full, but the load-bearing reason is local: iplane already ships
// a write timeout that severs long-running requests, and a channel held open
// for the life of an engine is that trap with no natural bound. A lease also
// survives a control-plane restart for free, since agents re-register on
// their normal cadence and the view refills within one lease period.
//
// This package does NOT replace the /health poller in internal/healthcheck.
// The poller drives quarantine, quarantine is router eligibility, and router
// eligibility is data-path correctness, which the design's guardrail keeps on
// pull. The registry carries membership, group composition and liveness. The
// two answer different questions and both are needed.
//
// CP/DP placement: control-plane code. It mutates state through the store and
// never touches the request hot path.
package engines

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DefaultLease is how long a registration stays valid without renewal, and
// therefore the worst-case delay before a dead engine is visible as LOST.
//
// 30s against a renewal cadence of a third of that leaves an agent two missed
// renewals of slack, so a single dropped request or a GC pause does not
// evict a healthy engine. Detection latency is a property of this number and
// not of fleet size, which is the point of leasing rather than polling.
const DefaultLease = 30 * time.Second

// RenewDivisor sets the renewal cadence the control plane asks agents for,
// as a fraction of the lease. Three gives two chances to renew before
// expiry.
const RenewDivisor = 3

// ErrNoID is returned when a registration arrives without an engine id.
// The id is engine-chosen and stable across restarts; without it the
// registry cannot tell a renewal from a second member.
var ErrNoID = errors.New("engines: registration requires engine.id")

// Store is the persistence the registry needs. Narrow on purpose so tests
// substitute a map and the real implementation stays the state file.
//
// Implementations must be safe for concurrent use: N agents renew
// independently and each renewal is a read-modify-write.
type Store interface {
	// PutEngine writes e, replacing any engine with the same id.
	PutEngine(e *provisionerv1.Engine) error
	// ListEngines returns every known engine, including LOST ones.
	ListEngines() ([]*provisionerv1.Engine, error)
}

// Registry records engine registrations and expires the ones that stop
// renewing.
type Registry struct {
	store Store
	lease time.Duration

	// now is the clock seam. Tests inject a fake so lease expiry is
	// exercised without sleeping.
	mu  sync.Mutex
	now func() time.Time
}

// Option configures a Registry.
type Option func(*Registry)

// WithLease overrides DefaultLease. Shorter means faster detection of a dead
// engine and more registration traffic; the trade is linear and the default
// is tuned for a demo-sized fleet.
func WithLease(d time.Duration) Option {
	return func(r *Registry) {
		if d > 0 {
			r.lease = d
		}
	}
}

// WithClock injects a time source. Tests use it to advance past a lease
// without sleeping; production leaves it at time.Now.
func WithClock(now func() time.Time) Option {
	return func(r *Registry) {
		if now != nil {
			r.now = now
		}
	}
}

// New returns a Registry over store.
func New(store Store, opts ...Option) *Registry {
	r := &Registry{store: store, lease: DefaultLease, now: time.Now}
	for _, o := range opts {
		o(r)
	}
	return r
}

// LeaseSeconds is the renewal deadline handed back to agents, in seconds.
func (r *Registry) LeaseSeconds() int32 {
	return int32(r.lease / time.Second)
}

// RenewInterval is the cadence the control plane wants agents to renew at.
func (r *Registry) RenewInterval() time.Duration {
	return r.lease / RenewDivisor
}

// Register records a registration or a renewal. The two are the same call
// because the difference is not useful to the caller: an engine that crashed
// and came back simply registers again, and an engine that never left is
// renewing. Idempotent on id.
//
// Control-plane-owned fields (registered_at, last_seen_at, lease_expires_at)
// are always overwritten, so a caller cannot extend its own lease by
// claiming a later expiry. registered_at is preserved across renewals so the
// operator can see how long an engine has been up.
//
// A LOST engine that registers again is revived rather than rejected: the
// control plane's conclusion that it was gone was a timeout, and the engine
// is the authority on its own existence.
func (r *Registry) Register(in *provisionerv1.Engine) (*provisionerv1.Engine, error) {
	if in.GetId() == "" {
		return nil, ErrNoID
	}
	if in.GetState() == provisionerv1.EngineState_ENGINE_STATE_LOST {
		return nil, fmt.Errorf("engines: %q cannot report itself LOST; that state is the "+
			"control plane's conclusion from an expired lease", in.GetId())
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	e := proto.Clone(in).(*provisionerv1.Engine)
	if e.GetState() == provisionerv1.EngineState_ENGINE_STATE_UNSPECIFIED {
		e.State = provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING
	}
	sortSpan(e)

	prior := r.lookupLocked(e.GetId())
	e.RegisteredAt = timestamppb.New(now)
	if prior != nil && prior.GetRegisteredAt() != nil {
		e.RegisteredAt = prior.GetRegisteredAt()
	}
	// A draining engine is still alive and still renewing while it finishes
	// in-flight work. Its own reported state is SERVING and always will be,
	// because the engine does not know an operator decided to reclaim it, so
	// honouring that report would un-drain the member on the next renewal.
	if prior.GetState() == provisionerv1.EngineState_ENGINE_STATE_DRAINING {
		e.State = provisionerv1.EngineState_ENGINE_STATE_DRAINING
	}
	e.LastSeenAt = timestamppb.New(now)
	e.LeaseExpiresAt = timestamppb.New(now.Add(r.lease))

	if err := r.store.PutEngine(e); err != nil {
		return nil, err
	}
	return e, nil
}

// SetState moves an engine to a state the control plane concluded rather than
// the engine reported: DRAINING when an operator takes it out of service,
// LOST when its lease expires.
//
// Renewal does not clobber it back. That matters for DRAINING specifically:
// the engine is still alive and still renewing while it finishes in-flight
// work, and a renewal arriving mid-drain must not un-drain it. Register()
// preserves a control-plane-owned state for exactly this reason.
//
// Returns an error if the engine is unknown, since silently succeeding would
// let a typo'd id read as a successful drain.
func (r *Registry) SetState(id string, state provisionerv1.EngineState) (*provisionerv1.Engine, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	cur := r.lookupLocked(id)
	if cur == nil {
		return nil, fmt.Errorf("engines: no engine with id %q", id)
	}
	next := proto.Clone(cur).(*provisionerv1.Engine)
	next.State = state
	if err := r.store.PutEngine(next); err != nil {
		return nil, err
	}
	return next, nil
}

// Get returns one engine by id, or nil when unknown.
func (r *Registry) Get(id string) (*provisionerv1.Engine, error) {
	all, err := r.store.ListEngines()
	if err != nil {
		return nil, err
	}
	for _, e := range all {
		if e.GetId() == id {
			return e, nil
		}
	}
	return nil, nil
}

// List returns known engines, LOST ones included, ordered by id so output is
// stable across calls. Filters are AND-ed; zero values mean no filter.
func (r *Registry) List(deploymentID string, state provisionerv1.EngineState) ([]*provisionerv1.Engine, error) {
	all, err := r.store.ListEngines()
	if err != nil {
		return nil, err
	}
	out := make([]*provisionerv1.Engine, 0, len(all))
	for _, e := range all {
		if deploymentID != "" && e.GetDeploymentId() != deploymentID {
			continue
		}
		if state != provisionerv1.EngineState_ENGINE_STATE_UNSPECIFIED && e.GetState() != state {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetId() < out[j].GetId() })
	return out, nil
}

// ExpireOverdue moves every engine whose lease has run out to LOST and
// returns how many it changed. Idempotent: an engine already LOST is left
// alone, so repeated sweeps do not churn the state file.
//
// Engines are marked rather than deleted. An operator debugging a fleet needs
// to see what went away, and a deleted row looks identical to one that never
// registered.
func (r *Registry) ExpireOverdue() (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	all, err := r.store.ListEngines()
	if err != nil {
		return 0, err
	}
	now := r.now()
	changed := 0
	for _, e := range all {
		if e.GetState() == provisionerv1.EngineState_ENGINE_STATE_LOST {
			continue
		}
		exp := e.GetLeaseExpiresAt()
		if exp == nil || !now.After(exp.AsTime()) {
			continue
		}
		lost := proto.Clone(e).(*provisionerv1.Engine)
		lost.State = provisionerv1.EngineState_ENGINE_STATE_LOST
		if err := r.store.PutEngine(lost); err != nil {
			return changed, err
		}
		changed++
	}
	return changed, nil
}

// lookupLocked finds an engine by id. Caller holds r.mu.
func (r *Registry) lookupLocked(id string) *provisionerv1.Engine {
	all, err := r.store.ListEngines()
	if err != nil {
		return nil
	}
	for _, e := range all {
		if e.GetId() == id {
			return e
		}
	}
	return nil
}

// sortSpan orders an engine's nodes by node_index so the fleet view renders
// them consistently regardless of the order the agent happened to send.
func sortSpan(e *provisionerv1.Engine) {
	span := e.GetSpan()
	sort.SliceStable(span, func(i, j int) bool {
		return span[i].GetNodeIndex() < span[j].GetNodeIndex()
	})
}

// TotalCards sums the GPUs across an engine's span. The number the fleet
// view's span column shows next to the node count.
func TotalCards(e *provisionerv1.Engine) int32 {
	var n int32
	for _, node := range e.GetSpan() {
		n += node.GetGpuCount()
	}
	return n
}
