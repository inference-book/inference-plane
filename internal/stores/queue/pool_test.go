package queue_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/stores/queue"
	"github.com/inference-book/inference-plane/internal/stores/queue/inmem"
)

func TestPool_DrainsItemsInFIFOWithOneServicer(t *testing.T) {
	q := inmem.New[int](16)
	var got []int
	var mu sync.Mutex

	pool := queue.NewPool[int](q, 1, func(_ context.Context, v int) {
		mu.Lock()
		got = append(got, v)
		mu.Unlock()
	})
	pool.Start(context.Background())
	defer pool.Stop()

	for i := 0; i < 8; i++ {
		if err := pool.Submit(i); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 8 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 8 {
		t.Fatalf("processed %d items, want 8: %v", len(got), got)
	}
	for i, v := range got {
		if v != i {
			t.Fatalf("got[%d]=%d, want %d (FIFO violated with single servicer)", i, v, i)
		}
	}
}

func TestPool_ServicersRunInParallel(t *testing.T) {
	const servicers = 4
	q := inmem.New[int](64)

	// inFlight tracks the simultaneous count; the test asserts it
	// reaches `servicers` at peak. Each handler blocks long enough to
	// guarantee all servicers grab work before the first one returns.
	var inFlight, peakInFlight int64
	var mu sync.Mutex
	hold := make(chan struct{})

	pool := queue.NewPool[int](q, servicers, func(_ context.Context, _ int) {
		current := atomic.AddInt64(&inFlight, 1)
		mu.Lock()
		if current > peakInFlight {
			peakInFlight = current
		}
		mu.Unlock()
		<-hold
		atomic.AddInt64(&inFlight, -1)
	})
	pool.Start(context.Background())

	for i := 0; i < servicers; i++ {
		if err := pool.Submit(i); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	// Wait for all servicers to be in the handler.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt64(&inFlight) == int64(servicers) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&inFlight); got != int64(servicers) {
		t.Fatalf("inFlight=%d, want %d (servicers not running in parallel)", got, servicers)
	}
	close(hold) // let handlers complete

	pool.Stop()
	mu.Lock()
	defer mu.Unlock()
	if peakInFlight != int64(servicers) {
		t.Fatalf("peakInFlight=%d, want %d", peakInFlight, servicers)
	}
}

func TestPool_SubmitFull_ReturnsErrQueueFull(t *testing.T) {
	q := inmem.New[int](2)
	hold := make(chan struct{})
	pool := queue.NewPool[int](q, 1, func(_ context.Context, _ int) {
		<-hold
	})
	pool.Start(context.Background())
	defer func() {
		close(hold)
		pool.Stop()
	}()

	// Submit until full. With 1 servicer holding the first item and
	// capacity 2 in the queue, slots 2 + 3 should fill the buffer, and
	// the 4th submit should return ErrQueueFull.
	//
	// Race-safety: the servicer may or may not have pulled the first
	// item by the time we submit the 2nd. So submitting 3 items is
	// guaranteed to either succeed (handler waits, queue has both) or
	// the servicer is processing first item with queue full + 1.
	// Either way, the 4th Submit must fail.
	for i := 0; i < 3; i++ {
		_ = pool.Submit(i) // best-effort fill
	}
	// Now wait a beat to let the servicer Pop one, then push until full.
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 8; i++ {
		_ = pool.Submit(100 + i)
	}
	err := pool.Submit(999)
	if !errors.Is(err, queue.ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull at saturation, got %v", err)
	}
}

func TestPool_Stop_AfterDrainSubmitReturnsErrClosed(t *testing.T) {
	q := inmem.New[int](4)
	processed := make(chan int, 4)
	pool := queue.NewPool[int](q, 2, func(_ context.Context, v int) {
		processed <- v
	})
	pool.Start(context.Background())

	for i := 0; i < 4; i++ {
		if err := pool.Submit(i); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	// Wait for drain.
	for i := 0; i < 4; i++ {
		select {
		case <-processed:
		case <-time.After(time.Second):
			t.Fatalf("processed only %d items before timeout", i)
		}
	}
	pool.Stop()

	if err := pool.Submit(99); !errors.Is(err, queue.ErrClosed) {
		t.Fatalf("submit after Stop: expected ErrClosed, got %v", err)
	}
}

func TestPool_Stop_Idempotent(t *testing.T) {
	q := inmem.New[int](2)
	pool := queue.NewPool[int](q, 1, func(_ context.Context, _ int) {})
	pool.Start(context.Background())
	pool.Stop()
	pool.Stop() // must not panic / hang
}

func TestPool_Stop_BeforeStartIsNoOp(t *testing.T) {
	q := inmem.New[int](2)
	pool := queue.NewPool[int](q, 1, func(_ context.Context, _ int) {})
	pool.Stop() // unstarted pool — must not hang
}

func TestPool_NewPool_PanicsOnInvalidArgs(t *testing.T) {
	q := inmem.New[int](2)
	noop := func(_ context.Context, _ int) {}

	cases := []struct {
		name      string
		q         queue.BoundedQueue[int]
		servicers int
		handler   func(context.Context, int)
	}{
		{"nil queue", nil, 1, noop},
		{"nil handler", q, 1, nil},
		{"zero servicers", q, 0, noop},
		{"negative servicers", q, -1, noop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("NewPool(%s) did not panic", tc.name)
				}
			}()
			_ = queue.NewPool[int](tc.q, tc.servicers, tc.handler)
		})
	}
}
