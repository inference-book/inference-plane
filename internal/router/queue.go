package router

import (
	"context"
	"errors"
	"net/http"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/scheduler"
	"github.com/inference-book/inference-plane/internal/stores/queue"
)

// queueEntry is the payload submitted to the scheduler when the
// router has a scheduler configured. The HTTP handler goroutine
// resolves the deployment + priority, then submits an entry and
// blocks on done; the scheduler worker pops, runs the full
// instrumented dispatch (`handleWithObservability`), and closes done.
//
// The ResponseWriter and Request are owned exclusively by whichever
// goroutine holds them at any given moment: handler owns them until
// Submit, scheduler worker owns them after dispatch starts. The
// handler's blocking on done keeps the underlying http.Server
// goroutine alive so the ResponseWriter stays valid for the worker's
// writes.
//
// TenantID + Priority are captured into struct fields (in addition
// to living on req's context) so Beat 2.5+ scheduler logic that
// inspects the queue without invoking the handler (e.g., shedding
// decisions, per-tenant/per-lane metrics) can read them without
// unmarshaling the context.
type queueEntry struct {
	w                 http.ResponseWriter
	req               *http.Request
	dep               *provisionerv1.Deployment
	stripDeployPrefix bool
	done              chan struct{}
	TenantID          string
	Priority          provisionerv1.Priority
}

// DeploymentID satisfies scheduler.Entry. The scheduler's
// in-flight cap uses this as the bucket key.
func (e *queueEntry) DeploymentID() string {
	if e.dep == nil {
		return ""
	}
	return e.dep.GetId()
}

// PriorityLabel satisfies scheduler.Entry's Priority() string
// method. Named PriorityLabel here so it doesn't shadow the
// queueEntry.Priority field (the typed proto enum).
func (e *queueEntry) priorityLabelString() string {
	return priorityLabel(e.Priority)
}

// Priority satisfies scheduler.Entry. Returns the string label
// (LaneInteractive | LaneBatch).
func (e *queueEntry) PriorityLabel() string {
	return e.priorityLabelString()
}

// schedulerEntry adapts queueEntry to scheduler.Entry. Doing it on
// the type itself would conflict with queueEntry.Priority (the proto
// enum field); a thin wrapper that exposes the right method names
// keeps the field naming clean. Method receivers, not pointer
// shenanigans, do the adaptation.
type schedulerEntry struct {
	*queueEntry
}

func (s schedulerEntry) Priority() string     { return s.priorityLabelString() }
func (s schedulerEntry) DeploymentID() string { return s.queueEntry.DeploymentID() }

// dispatchEntry is the scheduler-facing handler. Closes done after
// the proxy call returns. The scheduler's root context is
// intentionally NOT propagated into the proxy call -- the per-request
// context lives on entry.req and is what the proxy + tracer use.
// Scheduler ctx would cancel mid-request on daemon shutdown, mangling
// streaming responses; relying on the request ctx means
// client-disconnect cancels the upstream and server-shutdown drains
// in-flight cleanly.
func (r *Router) dispatchEntry(_ context.Context, e scheduler.Entry) {
	se := e.(schedulerEntry).queueEntry
	defer close(se.done)
	r.handleWithObservability(se.w, se.req, se.dep, se.stripDeployPrefix)
}

// enqueueOrServe is the fork point. It first resolves the effective
// priority for this request (header > deployment default >
// INTERACTIVE), stashes it on the request ctx + entry, then routes:
// if a scheduler is configured, Submit + block on done; otherwise
// dispatch inline (Beat 1 direct-forward path).
//
// On ErrQueueFull, returns 503 + Retry-After: 1. The chapter teaches
// this as the bounded-buffer backpressure shape (M/M/k/N -- arrivals
// beyond the buffer are rejected).
func (r *Router) enqueueOrServe(w http.ResponseWriter, req *http.Request, dep *provisionerv1.Deployment, stripDeployPrefix bool) {
	priority := effectivePriority(req.Context(), dep)
	// Re-stash on ctx so handleWithObservability reads the effective
	// (post-fallback) value, not the bare header value the middleware
	// stored. The two-write pattern matches withTenant + the inline
	// effectivePriority resolver.
	ctx := context.WithValue(req.Context(), priorityCtxKey{}, priority)
	req = req.WithContext(ctx)

	if r.scheduler == nil {
		r.handleWithObservability(w, req, dep, stripDeployPrefix)
		return
	}
	entry := &queueEntry{
		w:                 w,
		req:               req,
		dep:               dep,
		stripDeployPrefix: stripDeployPrefix,
		done:              make(chan struct{}),
		TenantID:          tenantFromContext(req.Context()),
		Priority:          priority,
	}
	if err := r.scheduler.Submit(schedulerEntry{entry}); err != nil {
		switch {
		case errors.Is(err, queue.ErrQueueFull):
			w.Header().Set("Retry-After", "1")
			writeOpenAIError(w, http.StatusServiceUnavailable,
				"router queue is full; retry shortly", "queue_full")
		case errors.Is(err, queue.ErrClosed),
			errors.Is(err, scheduler.ErrSchedulerClosed):
			writeOpenAIError(w, http.StatusServiceUnavailable,
				"router is shutting down", "router_shutting_down")
		default:
			writeOpenAIError(w, http.StatusInternalServerError,
				"router queue submit failed: "+err.Error(), "internal_error")
		}
		return
	}
	<-entry.done
}
