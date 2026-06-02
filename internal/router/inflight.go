package router

import (
	"context"
	"sync/atomic"
)

// trackInFlight wraps the dispatch + response window for one request.
// Increments the per-(deploy, replica) counter, records the gauge,
// and returns a deferrable that decrements the counter and records
// again. The two records bracket the request's lifetime so the
// iplane.replica.in_flight gauge transitions on both edges, matching
// the dashboard's "live load" expectation (v0.2 ch7-beat3.6, #88).
//
// Why a closure-returning helper rather than two distinct methods:
// callers can `defer r.trackInFlight(ctx, ...)()` and the lifetime
// is impossible to mis-pair (no "I incremented but forgot to
// decrement" bug). The defer-the-call shape matches the std-lib's
// `defer mu.Unlock()` idiom.
//
// nil-safe via the recorder's own nil guard; this method does the
// counter bookkeeping regardless so the int64 value stays accurate
// across no-telemetry test runs.
func (r *Router) trackInFlight(ctx context.Context, deployID, replicaID string) func() {
	key := deployID + "/" + replicaID
	counterAny, _ := r.inFlight.LoadOrStore(key, new(atomic.Int64))
	counter := counterAny.(*atomic.Int64)
	current := counter.Add(1)
	r.recorder.RecordReplicaInFlight(ctx, deployID, replicaID, current)
	return func() {
		current := counter.Add(-1)
		r.recorder.RecordReplicaInFlight(ctx, deployID, replicaID, current)
	}
}
