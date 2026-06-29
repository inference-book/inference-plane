package scheduler

import (
	"context"
	"errors"
	mathrand "math/rand/v2"
	"sort"
	"sync"

	"github.com/inference-book/inference-plane/internal/stores/queue"
	"github.com/inference-book/inference-plane/internal/stores/queue/inmem"
)

// laneState holds the per-tenant sub-queues for one priority lane
// plus the weighted-lottery selection state. v0.2 ch7-beat2.5
// introduced per-tenant fair-share dispatch: each lane (interactive,
// batch) splits into N sub-queues keyed by tenant_id; the worker
// loop picks one tenant per dispatch using weighted random
// selection.
//
// # Capacity semantics
//
// Each tenant has its OWN bounded waiting room of size perTenantCap.
// Total per-lane capacity is N_tenants * perTenantCap. The
// alternative (a single bounded buffer shared across tenants) would
// let one busy tenant squeeze others out under saturation, which
// defeats the purpose of fair-share. Per-tenant caps preserve
// isolation; the trade-off is N_tenants * cap memory which is
// bounded by the operator's known tenant population.
//
// # Selection algorithm (weighted lottery)
//
// On each tryPop:
//
//  1. Collect tenants whose sub-queue is non-empty.
//  2. Sum their weights.
//  3. Pick a uniformly-random integer in [0, totalWeight) and
//     find the tenant whose cumulative-weight interval contains it.
//  4. Pop one entry from that tenant's queue.
//
// Lottery beats DRR's credit machinery for simplicity (no
// per-tenant state to maintain across calls; no cursor index to
// manage under churn) and gives the same long-run weight ratio. The
// per-request ordering is non-deterministic, but the chapter
// narrative is "ratio under sustained load," which the lottery
// matches exactly.
type laneState struct {
	name         string
	weights      Weights
	perTenantCap int

	mu      sync.Mutex
	queues  map[string]queue.BoundedQueue[Entry]
	tenants []string // sorted; populated on first push from a new tenant
	closed  bool
}

func newLaneState(name string, weights Weights, perTenantCap int) *laneState {
	return &laneState{
		name:         name,
		weights:      weights,
		perTenantCap: perTenantCap,
		queues:       map[string]queue.BoundedQueue[Entry]{},
	}
}

// push routes an entry to the sub-queue keyed by tenantID. Lazily
// allocates the sub-queue on first push from a previously unseen
// tenant. Returns queue.ErrQueueFull when that tenant's sub-queue
// is at perTenantCap; queue.ErrClosed when the lane has been
// closed via Close.
func (ls *laneState) push(tenantID string, e Entry) error {
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return queue.ErrClosed
	}
	q, ok := ls.queues[tenantID]
	if !ok {
		q = inmem.New[Entry](ls.perTenantCap)
		ls.queues[tenantID] = q
		ls.tenants = append(ls.tenants, tenantID)
		sort.Strings(ls.tenants)
	}
	ls.mu.Unlock()
	return q.Push(e)
}

// tryPop runs the weighted-lottery selection and returns one entry
// from the picked tenant's sub-queue. Returns (zero, ctx.Canceled)
// when no sub-queue has items (the rootCtx is a pre-cancelled
// "non-blocking try" ctx supplied by the worker). Returns
// (zero, queue.ErrClosed) when the lane is closed AND every
// sub-queue is drained.
//
// The lottery is over CURRENTLY-NON-EMPTY tenants only, so empty
// sub-queues don't dilute the share of active ones. Under
// saturation (all tenants always have work), the ratio converges
// to the configured weight ratio.
func (ls *laneState) tryPop(rootCtx context.Context) (Entry, error) {
	type candidate struct {
		id     string
		weight int
		q      queue.BoundedQueue[Entry]
	}
	ls.mu.Lock()
	allClosedAndDrained := ls.closed
	var candidates []candidate
	totalWeight := 0
	for _, t := range ls.tenants {
		q := ls.queues[t]
		if q.Len() == 0 {
			continue
		}
		allClosedAndDrained = false
		w := ls.weights.WeightFor(t)
		candidates = append(candidates, candidate{id: t, weight: w, q: q})
		totalWeight += w
	}
	ls.mu.Unlock()

	if len(candidates) == 0 {
		if allClosedAndDrained {
			var zero Entry
			return zero, queue.ErrClosed
		}
		var zero Entry
		return zero, context.Canceled
	}

	// Weighted selection. math/rand/v2 is goroutine-safe.
	r := mathrand.IntN(totalWeight)
	cumulative := 0
	var chosen candidate
	for _, c := range candidates {
		cumulative += c.weight
		if r < cumulative {
			chosen = c
			break
		}
	}

	// Non-blocking try-pop on the chosen tenant's queue. The queue's
	// own ctx-cancel returns the item immediately if available; if
	// another worker raced us and emptied it, returns ctx.Canceled
	// and the worker loops to the next try.
	tryCtx, cancel := context.WithCancel(rootCtx)
	cancel()
	return chosen.q.Pop(tryCtx)
}

// totalLen returns the sum of sub-queue lengths -- the lane's
// total waiting depth across all tenants. Used by metric callbacks
// + tests; point-in-time observation.
func (ls *laneState) totalLen() int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	n := 0
	for _, q := range ls.queues {
		n += q.Len()
	}
	return n
}

// lenForTenant returns the depth of one tenant's sub-queue, or 0
// if the tenant has never submitted.
func (ls *laneState) lenForTenant(tenantID string) int {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if q, ok := ls.queues[tenantID]; ok {
		return q.Len()
	}
	return 0
}

// snapshotTenants returns a copy of the known tenant list. Used by
// async metric callbacks that iterate per-tenant depth.
func (ls *laneState) snapshotTenants() []string {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	out := make([]string, len(ls.tenants))
	copy(out, ls.tenants)
	return out
}

// close marks every sub-queue closed. Subsequent push calls return
// queue.ErrClosed; pop calls drain remaining items then return
// queue.ErrClosed once empty. Idempotent.
func (ls *laneState) close() {
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return
	}
	ls.closed = true
	queues := make([]queue.BoundedQueue[Entry], 0, len(ls.queues))
	for _, q := range ls.queues {
		queues = append(queues, q)
	}
	ls.mu.Unlock()
	for _, q := range queues {
		_ = q.Close()
	}
}

// errAllSubQueuesClosed is the sentinel tryPop returns when EVERY
// per-tenant sub-queue has reported ErrClosed -- meaning the lane
// is fully drained and the worker should exit (rather than spin on
// empty closed queues). Currently the tryPop path returns
// queue.ErrClosed directly when all are empty + closed; this var
// is kept as a hook for future variants.
var errAllSubQueuesClosed = errors.New("scheduler: lane fully drained")

var _ = errAllSubQueuesClosed // documented hook; not yet used
