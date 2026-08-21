// Helpers for reading what backs a Deployment.
//
// A Deployment's members live in dep.replicas, one entry per member,
// each holding the instances behind it and the single endpoint it
// serves on. These functions encapsulate two things callers should not
// re-derive: the fallback to the singular instance_id / engine_endpoint
// fields for a one-instance record, and the flattening of a member's
// instance set when a caller wants machines rather than members.
//
// Which helper to reach for is the question worth pausing on. A caller
// routing traffic wants MEMBERS, because a member is one engine on one
// endpoint however many machines it spans. A caller reconciling rentals
// or tearing down wants MACHINES, because every one of them bills.

package provisioners

import provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"

// EffectiveInstanceIDs returns every Instance backing a Deployment,
// flattened across members in provisioning order.
//
// Machines, not members: a four-node member contributes four ids. This
// is the list to iterate when the question is "what did we rent", which
// is what teardown, reconciliation and cost all ask.
//
// Returns nil only for a record with no members and no singular
// instance_id, which is a corrupt record rather than an empty one.
func EffectiveInstanceIDs(dep *provisionerv1.Deployment) []string {
	if dep == nil {
		return nil
	}
	if reps := dep.GetReplicas(); len(reps) > 0 {
		var out []string
		for _, r := range reps {
			out = append(out, r.GetInstanceIds()...)
		}
		if len(out) > 0 {
			return out
		}
	}
	if id := dep.GetInstanceId(); id != "" {
		return []string{id}
	}
	return nil
}

// EffectiveEndpoints returns one endpoint per member, in member order.
//
// Members, not machines. A member spanning four nodes contributes one
// endpoint, because one engine serves on one address whatever it is
// assembled from. Index i here is member i, and it lines up with
// EffectiveMemberInstanceIDs(dep)[i] rather than with
// EffectiveInstanceIDs.
//
// May contain empty strings for members still coming up. Callers that
// route traffic MUST skip them.
func EffectiveEndpoints(dep *provisionerv1.Deployment) []string {
	if dep == nil {
		return nil
	}
	if reps := dep.GetReplicas(); len(reps) > 0 {
		out := make([]string, len(reps))
		for i, r := range reps {
			out[i] = r.GetEngineEndpoint()
		}
		return out
	}
	if ep := dep.GetEngineEndpoint(); ep != "" {
		return []string{ep}
	}
	return nil
}

// EffectiveMemberInstanceIDs returns each member's primary instance id,
// in member order, so a caller with a member index can name it.
//
// The primary is the member's first instance: the id the member is
// known by and, for a multi-node engine, the rank-0 node. A caller
// asking "which replica is slot i" wants this; a caller asking "what is
// billing" wants EffectiveInstanceIDs.
func EffectiveMemberInstanceIDs(dep *provisionerv1.Deployment) []string {
	if dep == nil {
		return nil
	}
	if reps := dep.GetReplicas(); len(reps) > 0 {
		out := make([]string, 0, len(reps))
		for _, r := range reps {
			if ids := r.GetInstanceIds(); len(ids) > 0 {
				out = append(out, ids[0])
				continue
			}
			out = append(out, "")
		}
		return out
	}
	if id := dep.GetInstanceId(); id != "" {
		return []string{id}
	}
	return nil
}

// MemberOf returns the index of the member holding instanceID, or -1.
//
// The reverse lookup a slot patch needs: an update arrives naming an
// instance, and the record it belongs to is a member rather than a
// position in a flat list.
func MemberOf(dep *provisionerv1.Deployment, instanceID string) int {
	for i, r := range dep.GetReplicas() {
		for _, id := range r.GetInstanceIds() {
			if id == instanceID {
				return i
			}
		}
	}
	return -1
}
