package backends

import (
	"context"
	"fmt"
	"sync"
)

// kvBudget is a token-denominated admission gate, modelling the one
// resource that actually decides how many sequences an engine holds at
// once.
//
// The mock's latency is otherwise independent of everything: a request
// sleeps, and a hundred concurrent requests sleep in parallel, so
// throughput scales linearly for as long as the machine has goroutines.
// That is fine for the routing demos it was built for and useless for
// anything about capacity, because the interesting property of a real
// engine is that it cannot admit an unbounded batch. It has a fixed pool
// of KV cache, every sequence in flight holds a slice of it proportional
// to its length, and a sequence arriving when the pool is full waits.
//
// Which is why a concurrency ceiling falls as context rises, and it is
// the thing #341's arithmetic predicts. Without a budget here there is
// nothing to check that prediction against short of renting hardware.
//
// Weighted rather than counted, since that is the whole point. A gate
// admitting a fixed number of requests would cap concurrency at a number
// that does not move with context length, which is the behaviour this
// exists to avoid.
type kvBudget struct {
	mu    sync.Mutex
	cond  *sync.Cond
	total int
	used  int
}

func newKVBudget(total int) *kvBudget {
	b := &kvBudget{total: total}
	b.cond = sync.NewCond(&b.mu)
	return b
}

// acquire blocks until cost tokens are free, then reserves them.
//
// A request costing more than the whole pool is refused rather than
// parked forever, which is what a real engine does with a sequence longer
// than its configured maximum: no amount of waiting frees space that does
// not exist. The error names both numbers, since the operator's next move
// is to change one of them.
func (b *kvBudget) acquire(ctx context.Context, cost int) error {
	if cost <= 0 {
		return nil
	}
	if cost > b.total {
		return fmt.Errorf("request needs %d tokens of kv cache and the engine has %d in total", cost, b.total)
	}

	// Cond has no context-aware wait, so a cancelled caller is woken by
	// broadcasting on cancellation. Without this a request waiting on a
	// full pool ignores its deadline and the load driver's shutdown
	// hangs for as long as the slowest occupant.
	stop := context.AfterFunc(ctx, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		b.cond.Broadcast()
	})
	defer stop()

	b.mu.Lock()
	defer b.mu.Unlock()
	for b.used+cost > b.total {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.cond.Wait()
	}
	b.used += cost
	return nil
}

// release returns tokens to the pool.
func (b *kvBudget) release(cost int) {
	if cost <= 0 {
		return
	}
	b.mu.Lock()
	b.used -= cost
	if b.used < 0 {
		b.used = 0
	}
	b.mu.Unlock()
	b.cond.Broadcast()
}
