package router

import (
	"context"
	"errors"
	"net/http"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/stores/queue"
	"github.com/inference-book/inference-plane/internal/stores/queue/inmem"
)

// queueEntry is the payload pushed onto the M/M/k waiting room when
// the router is configured with servicers > 0. The HTTP handler
// goroutine resolves the deployment, then submits an entry and
// blocks on done; the servicer goroutine pops, runs the full
// instrumented dispatch (`handleWithObservability`), and closes done.
//
// The ResponseWriter and Request are owned exclusively by whichever
// goroutine holds them at any given moment: handler owns them until
// Submit, servicer owns them after Pop. The handler's blocking on
// done keeps the underlying http.Server goroutine alive so the
// ResponseWriter stays valid for the servicer's writes.
type queueEntry struct {
	w                 http.ResponseWriter
	req               *http.Request
	dep               *provisionerv1.Deployment
	stripDeployPrefix bool
	done              chan struct{}
}

// dispatchEntry is the handler the worker pool runs for each item.
// The pool's root context is intentionally NOT propagated into the
// proxy call — the per-request context lives on entry.req and is
// what the proxy + tracer use. Pool ctx would cancel mid-request on
// daemon shutdown, mangling streaming responses; relying on the
// request ctx means client-disconnect cancels the upstream and
// server-shutdown drains in-flight cleanly.
func (r *Router) dispatchEntry(entry *queueEntry) {
	defer close(entry.done)
	r.handleWithObservability(entry.w, entry.req, entry.dep, entry.stripDeployPrefix)
}

// enqueueOrServe is the fork point. When the pool is configured
// (servicers > 0), the entry is submitted to the queue and the
// handler goroutine blocks on done. Otherwise the entry is dispatched
// inline -- the Beat 1 direct-forward path.
//
// On ErrQueueFull, returns 503 + Retry-After: 1. The chapter teaches
// this as the bounded-buffer backpressure shape (M/M/k/N -- arrivals
// beyond the buffer are rejected).
func (r *Router) enqueueOrServe(w http.ResponseWriter, req *http.Request, dep *provisionerv1.Deployment, stripDeployPrefix bool) {
	if r.pool == nil {
		r.handleWithObservability(w, req, dep, stripDeployPrefix)
		return
	}
	entry := &queueEntry{
		w:                 w,
		req:               req,
		dep:               dep,
		stripDeployPrefix: stripDeployPrefix,
		done:              make(chan struct{}),
	}
	if err := r.pool.Submit(entry); err != nil {
		switch {
		case errors.Is(err, queue.ErrQueueFull):
			w.Header().Set("Retry-After", "1")
			writeOpenAIError(w, http.StatusServiceUnavailable,
				"router queue is full; retry shortly", "queue_full")
		case errors.Is(err, queue.ErrClosed):
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

// newPool constructs the worker pool that satisfies the M/M/k/N
// shape: a bounded inmem queue (capacity = waiting room) drained by
// servicers concurrent goroutines. Caller is responsible for Start
// and Stop lifecycle.
func newPool(servicers, capacity int, handler func(*queueEntry)) *queue.Pool[*queueEntry] {
	q := inmem.New[*queueEntry](capacity)
	return queue.NewPool[*queueEntry](q, servicers, func(_ context.Context, e *queueEntry) {
		handler(e)
	})
}
