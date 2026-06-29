package queue

import (
	"context"
	"errors"
	"sync"
)

// Pool runs N "servicer" goroutines, each in a loop pulling items
// from a BoundedQueue and calling the supplied handler. Together
// with a bounded waiting room (the queue) this is the textbook
// M/M/k/N shape: arrivals queue up to N items, then k goroutines
// drain at the rate the handler can sustain.
//
// Lifecycle:
//
//	pool := NewPool(q, 4, handler)
//	pool.Start(ctx)                  // spawns 4 servicer goroutines
//	pool.Submit(item)                // queue.Push delegate; ErrQueueFull on full
//	pool.Stop()                      // close queue + wait for servicers to exit
//
// Submit is a thin delegate to the underlying queue's Push. Stop
// closes the queue, which signals servicers to drain remaining items
// and exit cleanly. The pool's root context cancels when Stop is
// called so a stuck handler that respects ctx will unblock as well.
//
// Errors a handler emits are NOT propagated — the pool's job is to
// drain the queue. Handlers that need to report failure should write
// it back through the item's payload (typical pattern: a done
// channel + result fields on T).
type Pool[T any] struct {
	queue     BoundedQueue[T]
	handler   func(context.Context, T)
	servicers int

	mu     sync.Mutex
	rootCtx    context.Context
	rootCancel context.CancelFunc
	started    bool
	wg         sync.WaitGroup
}

// ErrInvalidServicers is returned by NewPool when servicers is not
// positive. The router treats servicers <= 0 as "no queue, direct
// forward" and skips Pool construction entirely; this sentinel is
// for callers that wire Pool directly and want a typed error.
var ErrInvalidServicers = errors.New("queue: pool servicers must be > 0")

// NewPool returns a Pool that will drain q with `servicers`
// goroutines, calling handler for each item. handler is invoked with
// the pool's root context (set by Start); item-scoped contexts (an
// http.Request's Context, say) belong inside T, not on the handler
// signature.
//
// NewPool does not start any goroutines — call Start to spawn them.
// This split lets callers register the pool, wire dependencies, and
// then activate it as part of the daemon's startup sequence.
//
// Panics if servicers <= 0 or queue/handler is nil. These are
// programmer errors and a nil-check at every call site would just
// move the panic later.
func NewPool[T any](q BoundedQueue[T], servicers int, handler func(context.Context, T)) *Pool[T] {
	if q == nil {
		panic("queue: NewPool: nil queue")
	}
	if handler == nil {
		panic("queue: NewPool: nil handler")
	}
	if servicers <= 0 {
		panic("queue: NewPool: servicers must be > 0")
	}
	return &Pool[T]{
		queue:     q,
		handler:   handler,
		servicers: servicers,
	}
}

// Start spawns the pool's servicer goroutines. parent is the
// caller's lifetime context — when it cancels, servicers exit
// after their in-flight handler invocation returns. Start returns
// immediately; the goroutines run until Stop is called or parent
// cancels.
//
// Idempotent guard: calling Start twice panics. The pool's lifetime
// is intentionally not re-entrant; reuse is via constructing a new
// Pool with the same queue.
func (p *Pool[T]) Start(parent context.Context) {
	p.mu.Lock()
	if p.started {
		p.mu.Unlock()
		panic("queue: Pool.Start: already started")
	}
	p.started = true
	p.rootCtx, p.rootCancel = context.WithCancel(parent)
	p.mu.Unlock()

	for i := 0; i < p.servicers; i++ {
		p.wg.Add(1)
		go p.servicerLoop()
	}
}

// servicerLoop is one servicer goroutine's body: Pop, hand off to the
// handler, repeat until the queue is drained-and-closed or the root
// ctx cancels. ErrClosed and ctx-cancel both cause clean exit.
//
// A panic in the handler is NOT caught — it bubbles up through the
// servicer goroutine and crashes the process. Recover here would hide
// real bugs; the production daemon catches panics in HTTP handlers
// upstream of Submit, before the item ever lands in the pool.
func (p *Pool[T]) servicerLoop() {
	defer p.wg.Done()
	for {
		item, err := p.queue.Pop(p.rootCtx)
		if err != nil {
			// ErrClosed (queue drained after Close) or ctx cancel.
			// Either way, this servicer is done.
			return
		}
		p.handler(p.rootCtx, item)
	}
}

// Submit pushes item onto the underlying queue. Returns ErrQueueFull
// when the bounded buffer is at capacity; the caller maps this to
// backpressure at the protocol layer. Submit before Start works —
// items queue up until servicers are running.
func (p *Pool[T]) Submit(item T) error {
	return p.queue.Push(item)
}

// Len returns the current number of items waiting in the pool's
// underlying queue. Point-in-time observation -- may be stale by the
// time the caller acts. Useful for per-lane depth metrics and tests.
func (p *Pool[T]) Len() int {
	return p.queue.Len()
}

// Stop closes the queue and waits for all servicers to drain
// remaining items and exit. After Stop, Submit returns ErrClosed.
//
// Stop cancels the pool's root context as well so handlers that
// respect ctx unblock; handlers that ignore ctx run to completion.
//
// Idempotent — calling Stop on an already-stopped pool is a no-op.
func (p *Pool[T]) Stop() {
	p.mu.Lock()
	if !p.started {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	_ = p.queue.Close()
	p.rootCancel()
	p.wg.Wait()
}
