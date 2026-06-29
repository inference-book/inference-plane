// Package scheduler is the v0.2 Beat 2.4 dequeue-and-dispatch layer:
// it sits between the router's queue lanes (Beat 2.3) and the
// engine, picking which entry runs next and enforcing a per-
// deployment in-flight cap.
//
// The scheduler is the chapter's central "control plane in the data
// path" primitive. Conceptually it's the seam between iplane's
// stream-shaping (queue + priorities + tenant + in-flight bound)
// and the engine's own continuous-batching scheduler -- iplane
// decides who-gets-the-next-slot; the engine decides how-to-batch
// the slots it gets.
//
// # Interface, not policy
//
// Scheduler is an interface so impls can be swapped: v0.2 ships
// InteractiveFirst with strict priority + per-deployment in-flight
// cap; v0.3 and beyond may layer WRR/DWRR/aging on top
// (tracked as a follow-up issue at the time #78 lands).
//
// The Entry interface keeps this package independent of
// internal/router: routers implement Entry on their request types.
package scheduler

import (
	"context"
	"errors"
)

// ErrSchedulerClosed is returned by Submit after Stop has been
// called. Mirrors queue.ErrClosed semantically so router code can
// switch on either without caring which layer signaled.
var ErrSchedulerClosed = errors.New("scheduler: closed")

// LaneInteractive and LaneBatch are the canonical lane names for
// the v0.2 priority classes. Implementations are free to support
// more lanes, but these two are the contract the router relies on.
const (
	LaneInteractive = "interactive"
	LaneBatch       = "batch"
)

// Entry is what the scheduler accepts. Routers attach this
// behavior to their per-request struct; the scheduler reads it
// without knowing the entry's concrete type.
//
// DeploymentID names the bucket the in-flight cap is enforced
// against -- two entries with the same DeploymentID share the
// concurrency budget for that deployment. Empty string is the
// "no bucket" sentinel: such entries dispatch without a cap.
//
// Priority returns LaneInteractive or LaneBatch (case-sensitive).
// Unrecognized values are treated as LaneInteractive by the
// default impl (fail-open into the high-priority lane); custom
// impls may differ.
type Entry interface {
	DeploymentID() string
	Priority() string
}

// HandlerFunc does the actual work for one dispatched entry --
// typically "forward to engine and write response back to the
// caller." The scheduler invokes it on its worker goroutine; the
// scheduler does NOT track the handler's success / failure --
// errors flow back through whatever channel the entry's payload
// carries (in the router's case, a done channel + response writer).
type HandlerFunc func(ctx context.Context, e Entry)

// Scheduler is the dequeue-and-dispatch contract. Submit lands
// entries on the right lane; Start spawns worker goroutines that
// dequeue and call the handler; Stop drains and exits.
//
// All methods are safe to call concurrently. Stop is idempotent;
// Submit after Stop returns ErrSchedulerClosed.
type Scheduler interface {
	Submit(e Entry) error
	Start(ctx context.Context)
	Stop()
	// Len returns the current waiting depth on the named lane,
	// or -1 if the lane is unknown to this impl. Used for
	// per-lane depth metrics (#80) and tests.
	Len(lane string) int
}
