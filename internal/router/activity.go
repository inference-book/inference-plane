package router

import (
	"sync"
	"sync/atomic"
	"time"
)

// lastCompletion records, per (deploy, replica), when that replica most
// recently *finished* a request.
//
// Deliberately completions rather than dispatches or in-flight count. An
// engine wedged mid-request holds its in-flight count high forever, so an
// in-flight signal would report a hung engine as the busiest one on the
// fleet. A completion is the only one of the three that cannot be produced
// by an engine that has stopped making progress.
//
// Values are unix nanoseconds in an *atomic.Int64 so the hot path stays a
// single store, and readers are health-check ticks rather than requests.
type lastCompletion struct {
	m sync.Map
}

// mark stamps now against a replica. Called on the completion edge of every
// routed request, successful or not: the question this answers is whether
// the engine is still turning requests around, not whether it liked them.
func (l *lastCompletion) mark(deployID, replicaID string, now time.Time) {
	key := deployID + "/" + replicaID
	v, _ := l.m.LoadOrStore(key, new(atomic.Int64))
	v.(*atomic.Int64).Store(now.UnixNano())
}

// since reports how long ago the replica last completed a request, and
// false when it has never completed one under this process.
//
// The bool is load-bearing and must not be collapsed into a large duration.
// "Never completed anything" and "completed something a long time ago" are
// different claims, and only the second one is evidence about the engine;
// the first is usually just a replica that has not been asked yet.
func (l *lastCompletion) since(deployID, replicaID string, now time.Time) (time.Duration, bool) {
	v, ok := l.m.Load(deployID + "/" + replicaID)
	if !ok {
		return 0, false
	}
	return now.Sub(time.Unix(0, v.(*atomic.Int64).Load())), true
}

// SinceLastCompletion reports how long ago a replica finished a request.
//
// Exists for the health checker, which uses it to veto a quarantine: a
// replica failing its /health probes while still completing requests is
// saturated rather than dead, and taking it out of service is what turns a
// slow deployment into a failed one. Returns false when this process has
// seen no completion for the replica, which the caller must read as "no
// evidence" rather than "idle" (#450).
func (r *Router) SinceLastCompletion(deployID, replicaID string, now time.Time) (time.Duration, bool) {
	return r.completions.since(deployID, replicaID, now)
}
