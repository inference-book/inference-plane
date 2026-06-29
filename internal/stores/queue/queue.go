// Package queue provides a bounded FIFO interface and an M/M/k worker
// pool that drains it. The router uses this in v0.2 Beat 2 to put a
// waiting room in front of the engine: N "servicer" goroutines pull
// the next request and forward it; arrivals beyond capacity fail
// fast with ErrQueueFull (the chapter teaches this as the M/M/k/N
// bounded-buffer shape).
//
// The package is generic over the payload type T. The router supplies
// a request-shaped struct; nothing in this package knows or cares
// about HTTP.
//
// # Why an interface for one impl
//
// `BoundedQueue` is an interface even though only the in-memory
// backend ships in v0.2. Two reasons:
//
//   - The Pool worker pool is generic and depends only on the
//     interface; alternate backends (redis, sqlite, persistent
//     across restarts) drop in by satisfying the same shape.
//   - It mirrors Beat 1's `internal/provisioners/stores/{file}/`
//     convention — interface at the top of the subpackage,
//     impls in sibling subdirs ("build the seam now, fill it later"
//     per ARCHITECTURE.md).
//
// # Pushdown target
//
// `BoundedQueue` + `Pool` are pure concurrency primitives with no
// iplane-specific assumptions. The intent is to extract both into
// `github.com/panyam/gocurrent` once the API has settled against
// real router usage — gocurrent already houses the project's
// concurrency-patterns library and this is the same shape category.
// See the follow-up issue filed alongside this PR.
package queue

import (
	"context"
	"errors"
)

// ErrQueueFull is returned by Push when a bounded queue is at
// capacity. Callers map this to backpressure at the protocol layer
// (the router returns 503 + Retry-After).
var ErrQueueFull = errors.New("queue: full")

// ErrClosed is returned by Push after Close, and by Pop when the
// queue has been closed AND drained. Pop on a closed-but-non-empty
// queue keeps returning items until empty; Pop on a closed-and-empty
// queue returns ErrClosed. This split lets workers drain a queue
// after shutdown is signaled.
var ErrClosed = errors.New("queue: closed")

// BoundedQueue is a FIFO with a fixed capacity. Push returns
// ErrQueueFull when the buffer is full; Pop blocks until an item is
// available, the context cancels, or the queue is closed-and-empty.
//
// Multi-producer, multi-consumer: implementations must be safe under
// concurrent Push and Pop from any number of goroutines.
type BoundedQueue[T any] interface {
	// Push inserts item at the tail of the queue. Returns
	// ErrQueueFull if the queue is at capacity, ErrClosed if the
	// queue has been closed. Push never blocks — the fail-fast
	// shape is load-bearing for the router's backpressure
	// contract (503 + Retry-After on full).
	Push(item T) error

	// Pop removes and returns the item at the head of the queue.
	// Blocks until an item is available or ctx is cancelled, in
	// which case it returns ctx.Err(). After Close has been called
	// AND the queue is drained, Pop returns the zero value of T
	// and ErrClosed.
	Pop(ctx context.Context) (T, error)

	// Len returns the current number of items in the queue.
	// Safe to call concurrently; result is a point-in-time
	// observation and may be stale by the time the caller acts on
	// it.
	Len() int

	// Cap returns the maximum number of items the queue can hold.
	Cap() int

	// Close marks the queue as no longer accepting new items.
	// Subsequent Push calls return ErrClosed. Pop calls return
	// remaining queued items in FIFO order, then ErrClosed once
	// drained. Idempotent — calling Close on an already-closed
	// queue is a no-op and returns nil.
	Close() error
}
