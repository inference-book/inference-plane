// Package policy is the routing-decision seam that decouples
// per-request replica selection from the rest of the router. v0.2
// ch7-beat3.9 ships round-robin as the only impl (extracted from
// PR for #85's inlined logic); Ch 8 drops in prefix-cache affinity
// as the second impl per iplane's "capability extracted when the
// 2nd impl appears" pattern.
//
// The seam exists so the data-plane router doesn't grow conditional
// branches for every future selection strategy. Today: RoundRobin.
// Tomorrow: PrefixAffinity (cache-aware). Day after: CostAware
// (prefer cheaper replicas in a heterogeneous fleet -- #143's
// payoff via the policy seam).
//
// Why an interface, not a switch statement:
//   - Policies have their own state (round-robin's counters, prefix-
//     affinity's cache view). State-per-policy doesn't fit on the
//     Router struct.
//   - Policies depend on different inputs (round-robin: nothing;
//     prefix-affinity: prompt hash from ctx; cost-aware: per-slot
//     provider/SKU). The interface accepts a wide context so each
//     policy can pull what it needs.
//   - Tests can swap a deterministic policy in to verify the seam
//     without exercising the chosen production policy's logic.
package policy

import (
	"context"
	"sync"
	"sync/atomic"
)

// Replica is the selectable unit. Carries identity (instance_id +
// endpoint) and is the lowest-common-denominator handed to every
// Policy. Future hetero-aware policies (cost-aware, region-aware)
// will read additional per-slot metadata; those fields land on
// Replica as the second consumer appears (the same "capability
// extracted when the 2nd impl appears" pattern that justified the
// seam itself).
//
// Today's RoundRobin reads only Endpoint to verify the slot is
// serviceable.
type Replica struct {
	// InstanceID is the deployment-relative replica id
	// (deploy_id-r0, -r1, ...). Stamped onto the request span's
	// iplane.router.replica_id attribute and the per-replica
	// metric labels.
	InstanceID string
	// Endpoint is the engine HTTP URL the router will forward to.
	// Always non-empty in the slice passed to Policy.Pick (the
	// router filters out empty + quarantined slots before calling).
	Endpoint string
}

// Stats is the per-replica runtime view a policy can query during
// Pick. Today's RoundRobin ignores it; Ch 8's prefix-affinity will
// read InFlight to break ties between equally-cache-hot replicas.
//
// Implementations live with the router (which already tracks
// in-flight per (deploy, replica) for the iplane.replica.in_flight
// gauge from #88). The policy package defines the interface so
// policy tests can substitute a fake.
type Stats interface {
	// InFlight returns the current in-flight request count for the
	// (deploy, replica) pair, as observed by the router. Returns 0
	// when the pair is unknown -- the router hasn't seen any
	// requests against this slot yet (or the slot was just added
	// by a scale-up).
	InFlight(deployID, replicaID string) int64
}

// Policy is the routing seam. Given a deployment's eligible replica
// set, pick one for this request. Empty endpoints and quarantined
// slots are filtered out before Pick is called -- policies see only
// what they can actually select.
//
// Returns ok=false when the eligible set is empty (the router maps
// this to 503 replica_unavailable -- the existing #85 contract).
//
// ctx carries the request's deadline + any policy-relevant hints
// (prompt hash for prefix-affinity, tenant id for tenant-aware
// routing, ...). Today's RoundRobin ignores ctx; future policies
// will pull keys via well-known context keys defined alongside
// their impl.
type Policy interface {
	Pick(ctx context.Context, deployID string, replicas []Replica, stats Stats) (selected Replica, ok bool)
}

// RoundRobin is the v0.2 default policy. Per-deployment atomic
// counter wraps modulo len(replicas); consecutive Pick calls cycle
// through the eligible set deterministically. Stateless across
// deployments (each deploy_id has its own counter).
//
// Behavior matches the inlined logic in PR for #85 before the
// extraction: ignores Stats, ignores ctx, ignores per-slot
// metadata. The 'plain' load balancer the chapter narrative
// describes; Ch 8's affinity policy will read the prompt hash
// from ctx and pick the cache-hot replica.
type RoundRobin struct {
	counters sync.Map // key: deployID, value: *atomic.Uint64
}

// NewRoundRobin constructs the v0.2 default policy. Used by
// router.New when no other policy is configured.
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{}
}

// Pick implements Policy. Increments the per-deployment counter
// unconditionally on each call, then returns replicas[counter % n].
// The atomic counter increment plus modulo gives a stable
// round-robin cycle under concurrent traffic.
func (rr *RoundRobin) Pick(_ context.Context, deployID string, replicas []Replica, _ Stats) (Replica, bool) {
	n := len(replicas)
	if n == 0 {
		return Replica{}, false
	}
	counterAny, _ := rr.counters.LoadOrStore(deployID, new(atomic.Uint64))
	counter := counterAny.(*atomic.Uint64)
	idx := int(counter.Add(1)-1) % n
	return replicas[idx], true
}
