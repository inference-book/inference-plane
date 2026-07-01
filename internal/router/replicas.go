package router

import (
	"context"
	"sync/atomic"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/router/policy"
)

// pickReplica selects one (instance_id, engine_endpoint) pair from
// the deployment's parallel lists via round-robin. v0.2 ch7-beat3.3
// (initial round-robin) + ch7-beat3.5 (#87, quarantine skip).
//
// Behavior:
//
//   - For Beat 1+2 single-instance Deployments (instance_ids empty,
//     singular instance_id + engine_endpoint set), the effective
//     helpers fall back to a single-entry slice and the loop picks
//     it every time. No behavior change.
//   - For Beat 3 multi-instance Deployments, an atomic per-deployment
//     counter wraps modulo n; consecutive requests visit consecutive
//     slots.
//   - Empty endpoint strings are skipped (the "instance still
//     provisioning" sentinel).
//   - Endpoints whose instance_id is in the deployment's
//     unhealthy_instance_ids set (the quarantine set written by
//     the health-poll loop, #87) are also skipped. Unlike empty
//     endpoints, quarantined replicas retain their endpoint URL --
//     the router just refuses to route to them while the
//     health-poll loop tracks recovery.
//   - If every slot is empty or quarantined, returns ok=false and
//     the caller writes 503 replica_unavailable.
//
// Returns the chosen replica's instance_id (for span attribute +
// metric label) and engine_endpoint (for proxyTo's reverse-proxy
// target).
//
// The atomic counter increments unconditionally per call -- even
// when we end up skipping that slot. This keeps the rotation
// monotonic and avoids the awkward "advance only on success"
// case where one always-empty slot would never let the rotation
// progress.
func (r *Router) pickReplica(ctx context.Context, dep *provisionerv1.Deployment) (instanceID, endpoint string, ok bool) {
	replicas := r.eligibleReplicas(dep)
	if len(replicas) == 0 {
		return "", "", false
	}
	selected, picked := r.policy.Pick(ctx, dep.GetId(), replicas, r)
	if !picked {
		return "", "", false
	}
	return selected.InstanceID, selected.Endpoint, true
}

// eligibleReplicas builds the policy-facing replica set: filters
// out empty endpoint slots (still-provisioning) and quarantined
// slots (#87). Policies see only what they can actually pick --
// the empty/quarantined filtering lives here, not in each policy,
// so future policies don't have to re-implement the same skip
// logic.
//
// Replica.InstanceID may be "" for legacy single-instance
// deployments where engine_endpoint is set but instance_id isn't
// populated. That's fine for routing -- the endpoint is what
// matters for forwarding; instance_id is metric/span metadata.
func (r *Router) eligibleReplicas(dep *provisionerv1.Deployment) []policy.Replica {
	eps := effectiveEndpoints(dep)
	if len(eps) == 0 {
		return nil
	}
	ids := effectiveInstanceIDs(dep)
	quarantined := quarantinedSet(dep)

	out := make([]policy.Replica, 0, len(eps))
	for i, ep := range eps {
		if ep == "" {
			continue
		}
		instanceID := ""
		if i < len(ids) {
			instanceID = ids[i]
		}
		if _, isQuarantined := quarantined[instanceID]; isQuarantined && instanceID != "" {
			continue
		}
		out = append(out, policy.Replica{
			InstanceID: instanceID,
			Endpoint:   ep,
		})
	}
	return out
}

// InFlight implements policy.Stats: returns the current in-flight
// request count for the (deploy, replica) pair from the router's
// own in-flight tracker (the same map that feeds the
// iplane.replica.in_flight gauge from #88). Returns 0 when the
// pair is unknown (the router hasn't seen any requests against
// the slot yet).
//
// Today's RoundRobin policy ignores Stats; Ch 8's prefix-affinity
// will read this to break ties between equally cache-hot replicas.
func (r *Router) InFlight(deployID, replicaID string) int64 {
	key := deployID + "/" + replicaID
	counterAny, ok := r.inFlight.Load(key)
	if !ok {
		return 0
	}
	return counterAny.(*atomic.Int64).Load()
}

// quarantinedSet builds the O(1)-lookup form of the deployment's
// unhealthy_instance_ids slice. Returns an empty map (not nil) when
// nothing is quarantined so the caller does not need a nil check.
// O(k) per pickReplica call where k = number of quarantined
// replicas (usually 0 or 1); cheaper than walking the slice once
// per round-robin iteration.
func quarantinedSet(dep *provisionerv1.Deployment) map[string]struct{} {
	ids := dep.GetUnhealthyInstanceIds()
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			out[id] = struct{}{}
		}
	}
	return out
}

// effectiveInstanceIDs returns the canonical list of Instance IDs
// backing a Deployment. Mirrors the helper in internal/provisioners
// (which the router can't import per CP/DP-1). Duplicated rather
// than shared via a new package: the function is 8 lines and the
// duplication is cheaper than a fourth package for two callers.
func effectiveInstanceIDs(dep *provisionerv1.Deployment) []string {
	if ids := dep.GetInstanceIds(); len(ids) > 0 {
		return ids
	}
	if id := dep.GetInstanceId(); id != "" {
		return []string{id}
	}
	return nil
}

// effectiveEndpoints is the parallel helper for engine endpoint URLs.
// engine_endpoints[i] corresponds to instance_ids[i]; empty string
// means "instance still provisioning or quarantined." Beat 1+2
// single-instance Deployments fall back to the singular
// engine_endpoint.
func effectiveEndpoints(dep *provisionerv1.Deployment) []string {
	if eps := dep.GetEngineEndpoints(); len(eps) > 0 {
		return eps
	}
	if ep := dep.GetEngineEndpoint(); ep != "" {
		return []string{ep}
	}
	return nil
}
