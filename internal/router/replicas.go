package router

import (
	"sync/atomic"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// pickReplica selects one (instance_id, engine_endpoint) pair from
// the deployment's parallel lists via round-robin. v0.2 ch7-beat3.3.
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
//     provisioning" or "future-quarantined" sentinel). If every
//     slot is empty, returns ok=false and the caller writes 503.
//
// Returns the chosen replica's instance_id (for span attribute +
// metric label) and engine_endpoint (for proxyTo's reverse-proxy
// target). When ok=false, all replicas are unhealthy / not-yet-ready
// and the caller responds with 503 replica_unavailable.
//
// The atomic counter increments unconditionally per call -- even
// when we end up skipping that slot. This keeps the rotation
// monotonic and avoids the awkward "advance only on success"
// case where one always-empty slot would never let the rotation
// progress.
func (r *Router) pickReplica(dep *provisionerv1.Deployment) (instanceID, endpoint string, ok bool) {
	eps := effectiveEndpoints(dep)
	if len(eps) == 0 {
		return "", "", false
	}
	ids := effectiveInstanceIDs(dep)

	counterAny, _ := r.rrCounters.LoadOrStore(dep.GetId(), new(atomic.Uint64))
	counter := counterAny.(*atomic.Uint64)

	// At most n attempts so a deployment with all empty slots fails
	// fast (rather than looping). The increment-and-modulo pattern
	// gives stable round-robin under concurrent traffic; visited
	// slots cycle deterministically across calls.
	//
	// ids may be shorter than eps if a deployment record has
	// engine_endpoint set without instance_id (test fixtures, or
	// pre-Beat-3 records that predate the instance_ids list). Pad
	// with empty replica_id labels rather than refusing to route --
	// the engine endpoint is what matters for forwarding; the
	// replica_id is metric/span metadata.
	n := len(eps)
	for range n {
		idx := int(counter.Add(1)-1) % n
		if eps[idx] != "" {
			instanceID := ""
			if idx < len(ids) {
				instanceID = ids[idx]
			}
			return instanceID, eps[idx], true
		}
	}
	return "", "", false
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
