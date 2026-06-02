package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/inference-book/inference-plane/internal/stores/queue"
	"github.com/inference-book/inference-plane/internal/stores/queue/inmem"
)

// InteractiveFirst is the v0.2 default Scheduler impl. Strict
// priority: interactive lane preferred; batch only runs when no
// interactive item is currently waiting. Within a lane, FIFO.
//
// # Worker loop
//
// Each worker round:
//
//  1. Try interactive queue non-blocking (Pop with a pre-cancelled
//     ctx returns the head item if present, or ctx.Canceled if
//     empty). On hit, dispatch and loop.
//  2. Try batch queue non-blocking. On hit, dispatch and loop.
//  3. Both empty: sleep briefly (exponential backoff capped at
//     idleMaxBackoff). On the next tick, try again.
//
// Direct queue inspection (rather than a feeder-channel design)
// guarantees strict priority deterministically: the worker always
// looks at interactive FIRST before considering batch. The cost is
// a small idle-CPU footprint when both lanes are empty (a few µs
// of polling per worker every idleMaxBackoff). Demo 05 and any
// real workload keep the workers busy enough that the backoff
// never hits ceiling.
//
// # Starvation
//
// # Architecture
//
//	         ┌──────────────┐         ┌──────────────┐
//	 Submit -┤ interactive  ├──feeder->┤  workers (k) ├─ HandlerFunc
//	         │  BoundedQueue│         │              │
//	         └──────────────┘         │  select:     │
//	         ┌──────────────┐         │   try int    │
//	 Submit -┤    batch     ├──feeder->┤   else any   │
//	         │  BoundedQueue│         │              │
//	         └──────────────┘         └──────────────┘
//	                                        │
//	                                        ▼
//	                              per-deployment semaphore
//	                              (in-flight cap)
//
// Two feeder goroutines bridge the BoundedQueue.Pop API into Go
// channels so the workers' strict-priority select can express
// "try interactive non-blocking; fall back to either lane." The
// feeders are scheduler-owned (one per lane) and exit when the
// underlying queue closes.
//
// # In-flight cap
//
// Before invoking the handler, a worker acquires a slot from the
// deployment's semaphore. When cap is 0 (the unlimited default),
// no semaphore is consulted -- the workers themselves are the only
// concurrency bound. cap > 0 caps concurrent in-flight to that
// many entries PER DEPLOYMENT ID. Workers wait when their target's
// semaphore is saturated and yield to other deployments naturally.
//
// # Starvation
//
// Strict priority CAN starve batch under sustained interactive
// load. This is the chapter narrative ("interactive cuts ahead of
// batch") and the v0.2 demo 05 talking point. Starvation
// prevention (WRR / DWRR / aging) is filed as a separate ticket
// and would land as a sibling Scheduler impl.
type InteractiveFirst struct {
	workers     int
	handler     HandlerFunc
	inFlightCap int

	interactive queue.BoundedQueue[Entry]
	batch       queue.BoundedQueue[Entry]

	// Lifecycle. Start sets rootCtx + spawns goroutines; Stop
	// cancels rootCtx, closes queues, waits.
	mu         sync.Mutex
	rootCtx    context.Context
	rootCancel context.CancelFunc
	started    bool
	wg         sync.WaitGroup

	// Per-deployment in-flight slots. Lazily created on first
	// dispatch to a given deployment id. When inFlightCap == 0
	// this map stays empty and the workers skip the acquire path.
	semMu sync.Mutex
	sems  map[string]chan struct{}
}

// idle backoff bounds. Workers poll the queues directly; when both
// are empty they sleep `idleStartBackoff` and double up to
// idleMaxBackoff. Active workloads never reach the cap.
const (
	idleStartBackoff = time.Millisecond
	idleMaxBackoff   = 10 * time.Millisecond
)

// InteractiveFirstConfig configures the scheduler at construction.
type InteractiveFirstConfig struct {
	// Workers is k: the number of concurrent dispatcher goroutines.
	// Each worker contributes 1 to the global engine in-flight bound
	// (if the per-deployment cap doesn't reduce it further).
	Workers int

	// InteractiveCapacity / BatchCapacity bound each lane's
	// waiting room. Submit returns queue.ErrQueueFull when the
	// matching lane is at capacity.
	InteractiveCapacity int
	BatchCapacity       int

	// InFlightCap is the per-deployment concurrent in-flight bound.
	// 0 means unlimited (workers themselves are the only bound).
	// Mirrors the engine's `max-num-seqs` so iplane stops feeding
	// the engine more than it batches in one tick.
	InFlightCap int

	// Handler is invoked for every dispatched entry. The
	// scheduler does not track its outcome; callers signal
	// success/failure through the entry's payload (e.g., a done
	// channel on the router's queueEntry).
	Handler HandlerFunc
}

// NewInteractiveFirst constructs a scheduler with the given
// config. Panics on invalid input -- nil handler, non-positive
// workers, non-positive lane capacity, negative in-flight cap --
// because these are programmer errors and a nil-check at every
// dispatch would just move the panic later.
func NewInteractiveFirst(cfg InteractiveFirstConfig) *InteractiveFirst {
	if cfg.Handler == nil {
		panic("scheduler: handler must not be nil")
	}
	if cfg.Workers <= 0 {
		panic("scheduler: workers must be > 0")
	}
	if cfg.InteractiveCapacity <= 0 {
		panic("scheduler: interactive capacity must be > 0")
	}
	if cfg.BatchCapacity <= 0 {
		panic("scheduler: batch capacity must be > 0")
	}
	if cfg.InFlightCap < 0 {
		panic("scheduler: in-flight cap must be >= 0 (0 = unlimited)")
	}
	return &InteractiveFirst{
		workers:     cfg.Workers,
		handler:     cfg.Handler,
		inFlightCap: cfg.InFlightCap,
		interactive: inmem.New[Entry](cfg.InteractiveCapacity),
		batch:       inmem.New[Entry](cfg.BatchCapacity),
		sems:        map[string]chan struct{}{},
	}
}

// Submit routes the entry to the matching lane's bounded queue.
// LaneBatch goes to the batch queue; everything else (including
// LaneInteractive and unrecognized priorities) goes to the
// interactive queue -- the safer default for unknown values.
func (s *InteractiveFirst) Submit(e Entry) error {
	if e.Priority() == LaneBatch {
		return s.batch.Push(e)
	}
	return s.interactive.Push(e)
}

// Start spawns `workers` dispatcher goroutines. Idempotent guard:
// calling Start twice panics; reuse is via constructing a new
// scheduler.
func (s *InteractiveFirst) Start(parent context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		panic("scheduler: Start called twice")
	}
	s.started = true
	s.rootCtx, s.rootCancel = context.WithCancel(parent)
	s.mu.Unlock()

	for range s.workers {
		s.wg.Add(1)
		go s.workerLoop()
	}
}

// tryPop returns the head of q if available, without blocking. Uses
// a pre-cancelled ctx so the underlying BoundedQueue.Pop returns
// immediately whether the queue has an item or is empty.
func (s *InteractiveFirst) tryPop(q queue.BoundedQueue[Entry]) (Entry, error) {
	tryCtx, cancel := context.WithCancel(s.rootCtx)
	cancel()
	return q.Pop(tryCtx)
}

// workerLoop is the strict-priority dispatch loop. Each round
// directly inspects the queues: interactive first, then batch.
// When both are empty, exponential backoff up to idleMaxBackoff.
// Strict priority is deterministic -- the worker always tries the
// higher-priority queue first before falling back to lower.
func (s *InteractiveFirst) workerLoop() {
	defer s.wg.Done()
	interactiveOpen, batchOpen := true, true
	backoff := idleStartBackoff
	for interactiveOpen || batchOpen {
		// Cheap ctx-cancel check up front; avoids one extra
		// round of polling on shutdown.
		select {
		case <-s.rootCtx.Done():
			return
		default:
		}

		// Phase 1: strict-priority try interactive.
		if interactiveOpen {
			item, err := s.tryPop(s.interactive)
			if err == nil {
				backoff = idleStartBackoff
				s.dispatch(item)
				continue
			}
			if errors.Is(err, queue.ErrClosed) {
				interactiveOpen = false
			}
			// Any other err (ctx.Canceled from try) -> empty;
			// fall through to phase 2.
		}

		// Phase 2: try batch.
		if batchOpen {
			item, err := s.tryPop(s.batch)
			if err == nil {
				backoff = idleStartBackoff
				s.dispatch(item)
				continue
			}
			if errors.Is(err, queue.ErrClosed) {
				batchOpen = false
			}
		}

		// Both empty (or closed): idle backoff.
		if !interactiveOpen && !batchOpen {
			return
		}
		select {
		case <-time.After(backoff):
			backoff *= 2
			if backoff > idleMaxBackoff {
				backoff = idleMaxBackoff
			}
		case <-s.rootCtx.Done():
			return
		}
	}
}

// dispatch acquires the per-deployment in-flight slot (if cap
// configured), invokes the handler, releases the slot. When cap
// is 0 (unlimited), the semaphore path is skipped entirely.
//
// Acquire blocks on ctx; if rootCtx cancels mid-acquire the
// worker exits its current round without invoking the handler.
// Entries already popped from the queue but never dispatched are
// dropped on Stop -- consistent with Pool's existing semantics.
func (s *InteractiveFirst) dispatch(e Entry) {
	if s.inFlightCap > 0 {
		sem := s.semaphoreFor(e.DeploymentID())
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-s.rootCtx.Done():
			return
		}
	}
	s.handler(s.rootCtx, e)
}

// semaphoreFor lazily creates the per-deployment in-flight slot
// channel. Capacity == inFlightCap. Slot acquire is `sem <- struct{}{}`
// (send blocks when full); release is `<-sem` (receive frees one).
// Inverted from the usual Acquire/Release direction because Go's
// "channel as semaphore" pattern reads more naturally this way
// (sending == taking a slot; receiving == returning a slot).
func (s *InteractiveFirst) semaphoreFor(deployID string) chan struct{} {
	s.semMu.Lock()
	defer s.semMu.Unlock()
	if existing, ok := s.sems[deployID]; ok {
		return existing
	}
	ch := make(chan struct{}, s.inFlightCap)
	s.sems[deployID] = ch
	return ch
}

// Stop closes both lane queues, cancels the root context, and
// waits for feeders + workers to exit. Idempotent.
//
// In-flight handler invocations are NOT aborted -- they run to
// completion (or until they observe the ctx cancellation
// themselves). Items popped from the queue but not yet handed to
// the handler are dropped. Items still in the queue at Stop time
// drain through the feeders + workers via the same close-then-
// finish-pending sequence the BoundedQueue contract guarantees.
func (s *InteractiveFirst) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	_ = s.interactive.Close()
	_ = s.batch.Close()
	s.rootCancel()
	s.wg.Wait()
}

// Len returns the current waiting depth on the named lane. Other
// lane names return -1. Point-in-time observation; may be stale by
// the time the caller acts on it.
func (s *InteractiveFirst) Len(lane string) int {
	switch lane {
	case LaneInteractive:
		return s.interactive.Len()
	case LaneBatch:
		return s.batch.Len()
	default:
		return -1
	}
}
