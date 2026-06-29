// Helpers for v0.2 ch7-beat3 multi-instance Deployments.
//
// These functions encapsulate the empty-list-falls-back-to-singular
// shim that lets v0.2-early (1.2 schema) and v0.2-late (1.3 schema)
// records coexist. Callers iterate through the helper rather than
// branching on emptiness themselves.

package provisioners

import provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"

// EffectiveInstanceIDs returns the canonical list of Instance IDs
// backing a Deployment. When dep.instance_ids is populated
// (multi-instance deployments after Beat 3 fan-out), it's returned
// verbatim. When empty (Beat 1+2 single-instance deployments or
// legacy 1.2-schema records), it falls back to a single-entry slice
// containing dep.instance_id.
//
// Always returns at least one entry for a well-formed Deployment;
// returns an empty slice only if both the list AND the singular
// instance_id are empty (corrupt record).
func EffectiveInstanceIDs(dep *provisionerv1.Deployment) []string {
	if dep == nil {
		return nil
	}
	if ids := dep.GetInstanceIds(); len(ids) > 0 {
		out := make([]string, len(ids))
		copy(out, ids)
		return out
	}
	if id := dep.GetInstanceId(); id != "" {
		return []string{id}
	}
	return nil
}

// EffectiveEndpoints returns the canonical list of engine endpoint
// URLs for a Deployment, parallel to EffectiveInstanceIDs.
// engine_endpoints[i] is the endpoint for instance_ids[i]. When
// dep.engine_endpoints is populated (Beat 3 fan-out), returned
// verbatim. When empty (Beat 1+2 single-instance) but the singular
// dep.engine_endpoint is set, falls back to a single-entry slice.
//
// May return entries that are the empty string -- a position in the
// list for an instance that's still provisioning (engine not yet
// reachable). Callers that route traffic MUST skip empties.
func EffectiveEndpoints(dep *provisionerv1.Deployment) []string {
	if dep == nil {
		return nil
	}
	if eps := dep.GetEngineEndpoints(); len(eps) > 0 {
		out := make([]string, len(eps))
		copy(out, eps)
		return out
	}
	if ep := dep.GetEngineEndpoint(); ep != "" {
		return []string{ep}
	}
	return nil
}
