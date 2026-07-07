package router

import "context"

// recordAffinity records whether this request's session landed on the
// same replica it last used ("hit") or moved / was first seen ("miss"),
// then updates the observation. A routing-locality signal measured
// independent of the active policy: round-robin scatters a session's
// turns across replicas (misses), prefix_affinity pins them (hits).
//
// First-seen counts as a miss: a session's opening turn has no prior
// replica, so there is genuinely no warm prefix to reuse. That makes
// the hit-rate climb from 0 as a conversation lengthens under affinity.
//
// See the iplane.router.affinity.total metric for how this proxies the
// engine's own prefix-cache hit-rate.
func (r *Router) recordAffinity(ctx context.Context, deployID, session, replicaID string) {
	key := deployID + "\x00" + session
	outcome := "miss"
	if prev, ok := r.sessionLastReplica.Load(key); ok && prev.(string) == replicaID {
		outcome = "hit"
	}
	r.sessionLastReplica.Store(key, replicaID)
	r.recorder.RecordRouterAffinity(ctx, deployID, outcome)
}
