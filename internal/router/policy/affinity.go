package policy

import (
	"context"
	"sync"
)

// sessionCtxKey is the unexported context key under which the router
// stashes the request's X-IPlane-Session value for PrefixAffinity to
// read. Defined here (not in the router) so the policy owns its own
// input contract: the router imports policy, never the reverse.
type sessionCtxKey struct{}

// WithSession returns a context carrying the session affinity key. The
// router calls this before Pick. An empty id is allowed --
// SessionFromContext returns "" and PrefixAffinity falls back to
// round-robin.
func WithSession(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionCtxKey{}, id)
}

// SessionFromContext returns the session affinity key stashed by
// WithSession, or "" when absent (no header, or a non-affinity caller
// that passed a plain context).
func SessionFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(sessionCtxKey{}).(string)
	return id
}

// PrefixAffinity pins a session's requests to the replica that already
// holds its prefix, so a multi-turn conversation's later turns hit a
// warm prefix cache instead of re-prefilling from cold on a fresh
// replica. It predicts residency from its own routing history (the pin
// map); it never queries the engine.
//
// Contract:
//   - No session key on the context -> fall back to round-robin (no
//     session, no affinity).
//   - New session -> place via the round-robin fallback, then pin.
//     Load-aware placement and the overload override that breaks a hot
//     pin are deferred to the load-aware-tie-break work.
//   - Returning session whose pinned replica is still eligible -> return
//     it (affinity hit).
//   - Returning session whose pinned replica is gone (quarantined /
//     scaled away) -> transparently re-pin to an eligible replica.
//   - Empty eligible set -> ok=false (router maps to 503).
type PrefixAffinity struct {
	fallback *RoundRobin

	// overloadThreshold enables the load-aware override: when a session's
	// pinned replica has this many or more in-flight requests, the turn
	// spills to the coolest eligible replica (the pin is kept, so the
	// session snaps back once its replica cools). 0 disables the override
	// -- pure stickiness. Absolute in-flight rather than relative so the
	// operator sets a concrete per-replica ceiling.
	overloadThreshold int64

	mu sync.Mutex
	// pins maps deployID + "\x00" + sessionID -> the pinned replica.
	// Unbounded in session count -- fine for the demo's bounded runs; a
	// bounded/LRU map is a follow-up under the ROADMAP "prefix-map
	// fidelity" open design question.
	pins map[string]Replica
}

// NewPrefixAffinity constructs the Ch 8 sticky-routing policy. Installed
// via router.WithRoutingPolicy when router.routing_policy is
// "prefix_affinity". overloadThreshold enables the load-aware override
// (see the field doc); 0 disables it for pure stickiness.
func NewPrefixAffinity(overloadThreshold int) *PrefixAffinity {
	return &PrefixAffinity{
		fallback:          NewRoundRobin(),
		overloadThreshold: int64(overloadThreshold),
		pins:              make(map[string]Replica),
	}
}

// Pick implements Policy. See PrefixAffinity for the routing contract.
func (p *PrefixAffinity) Pick(ctx context.Context, deployID string, replicas []Replica, stats Stats) (Replica, bool) {
	if len(replicas) == 0 {
		return Replica{}, false
	}
	session := SessionFromContext(ctx)
	if session == "" {
		return p.fallback.Pick(ctx, deployID, replicas, stats)
	}

	key := deployID + "\x00" + session

	p.mu.Lock()
	defer p.mu.Unlock()

	if pinned, ok := p.pins[key]; ok {
		if live, found := matchReplica(pinned, replicas); found {
			// Load-aware override: when the pinned replica is hot, spill
			// this turn to the coolest eligible replica WITHOUT dropping
			// the pin -- a temporary detour, not a re-pin, so the session
			// returns to its warm replica once load subsides. If the pin
			// is itself the coolest, leastInFlight returns it and there's
			// no actual spill.
			if p.overloadThreshold > 0 && stats != nil &&
				stats.InFlight(deployID, live.InstanceID) >= p.overloadThreshold {
				return leastInFlight(deployID, replicas, stats), true
			}
			return live, true
		}
		// Pinned replica no longer eligible; fall through and re-pin.
	}

	selected, ok := p.fallback.Pick(ctx, deployID, replicas, stats)
	if !ok {
		return Replica{}, false
	}
	p.pins[key] = selected
	return selected, true
}

// leastInFlight returns the eligible replica with the lowest in-flight
// count (ties broken by slice order). The load-aware override uses it to
// spill a hot session to the coolest replica. Callers guard stats != nil
// and len(replicas) > 0.
func leastInFlight(deployID string, replicas []Replica, stats Stats) Replica {
	best := replicas[0]
	bestN := stats.InFlight(deployID, best.InstanceID)
	for _, r := range replicas[1:] {
		if n := stats.InFlight(deployID, r.InstanceID); n < bestN {
			best, bestN = r, n
		}
	}
	return best
}

// matchReplica finds pinned in the live eligible set. Matches on
// InstanceID when the pin carries one (stable across endpoint churn),
// else on Endpoint (legacy single-instance deployments where InstanceID
// is unpopulated).
func matchReplica(pinned Replica, replicas []Replica) (Replica, bool) {
	for _, r := range replicas {
		if pinned.InstanceID != "" {
			if r.InstanceID == pinned.InstanceID {
				return r, true
			}
			continue
		}
		if r.Endpoint == pinned.Endpoint {
			return r, true
		}
	}
	return Replica{}, false
}
