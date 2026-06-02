// Package inmem is the in-process FIFO implementation of
// queue.BoundedQueue. Backed by a Go slice + condition variable; no
// external dependencies, no persistence. Suitable for single-process
// `iplane serve` deployments where queue state lives only as long as
// the daemon does.
//
// The condition-variable shape (sync.Cond) is used rather than a
// buffered channel because BoundedQueue's contract requires:
//
//   - Fail-fast Push (channel send blocks when full; we need
//     immediate ErrQueueFull).
//   - Pop that blocks on ctx (channel receive cannot cancel on
//     ctx without a select wrapper that complicates the
//     queue-drained-after-close case).
//   - Close semantics that drain remaining items then signal
//     ErrClosed (channel close signals immediately, doesn't drain).
//
// A buffered channel could mostly satisfy these with shim code, but
// the cond-var version expresses the queue's actual state machine
// cleanly: a slice with two waiters (push-blocked goroutines on full,
// pop-blocked goroutines on empty), woken when state changes.
package inmem

import (
	"context"
	"sync"

	"github.com/inference-book/inference-plane/internal/stores/queue"
)

// Queue is an in-memory bounded FIFO. Construct with New.
//
// Implements queue.BoundedQueue[T].
type Queue[T any] struct {
	capacity int

	mu       sync.Mutex
	notEmpty *sync.Cond // signaled when an item is pushed OR the queue is closed
	items    []T
	closed   bool
}

// New returns a Queue with the given capacity. Capacity must be
// positive; the constructor panics on capacity <= 0 because the
// alternative (silently treating 0 as "infinite" or as "always
// full") is more surprising than a fail-fast at startup. Callers
// (e.g., router config validation) should validate ahead of New.
func New[T any](capacity int) *Queue[T] {
	if capacity <= 0 {
		panic("inmem: capacity must be > 0")
	}
	q := &Queue[T]{
		capacity: capacity,
		items:    make([]T, 0, capacity),
	}
	q.notEmpty = sync.NewCond(&q.mu)
	return q
}

// Push inserts item at the tail of the queue. Returns ErrQueueFull
// when the queue is at capacity, ErrClosed if the queue has been
// closed.
func (q *Queue[T]) Push(item T) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return queue.ErrClosed
	}
	if len(q.items) >= q.capacity {
		return queue.ErrQueueFull
	}
	q.items = append(q.items, item)
	q.notEmpty.Signal()
	return nil
}

// Pop removes and returns the item at the head of the queue. Blocks
// until an item is available or ctx cancels. Returns the zero value
// of T and ErrClosed after Close once the queue is drained.
//
// The implementation uses a goroutine-leak-safe pattern: a watcher
// goroutine fires q.notEmpty.Broadcast() when ctx cancels, which
// kicks any Wait calls so they can re-check the loop condition and
// return ctx.Err(). The watcher exits as soon as Pop returns
// (signaled via the local `done` channel), so a long-lived ctx with
// many fast Pops doesn't accumulate watchers.
func (q *Queue[T]) Pop(ctx context.Context) (T, error) {
	var zero T

	// Set up the ctx-cancel waker. The watcher closes when this Pop
	// returns (whether by success, cancel, or close).
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			q.notEmpty.Broadcast()
			q.mu.Unlock()
		case <-done:
		}
	}()

	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		if len(q.items) > 0 {
			item := q.items[0]
			// Shift left rather than slice off the front; keeps the
			// underlying array bounded and prevents the slice's
			// backing array from growing unboundedly across
			// push/pop cycles.
			copy(q.items, q.items[1:])
			q.items = q.items[:len(q.items)-1]
			return item, nil
		}
		if q.closed {
			return zero, queue.ErrClosed
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		q.notEmpty.Wait()
	}
}

// Len returns the current number of items in the queue.
func (q *Queue[T]) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Cap returns the queue's fixed capacity.
func (q *Queue[T]) Cap() int {
	return q.capacity
}

// Close marks the queue as closed and wakes any pending Pop waiters.
// Already-queued items remain readable until drained; subsequent
// Push calls return ErrClosed. Idempotent.
func (q *Queue[T]) Close() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil
	}
	q.closed = true
	q.notEmpty.Broadcast()
	return nil
}
