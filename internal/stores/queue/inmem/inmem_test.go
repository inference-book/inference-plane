package inmem

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/stores/queue"
)

func TestQueue_PushPop_FIFOOrder(t *testing.T) {
	q := New[int](8)
	defer q.Close()

	for i := 0; i < 5; i++ {
		if err := q.Push(i); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if got := q.Len(); got != 5 {
		t.Fatalf("Len=%d, want 5", got)
	}
	if got := q.Cap(); got != 8 {
		t.Fatalf("Cap=%d, want 8", got)
	}

	for i := 0; i < 5; i++ {
		got, err := q.Pop(context.Background())
		if err != nil {
			t.Fatalf("pop %d: %v", i, err)
		}
		if got != i {
			t.Fatalf("pop %d: got %d, want %d (FIFO violated)", i, got, i)
		}
	}
	if got := q.Len(); got != 0 {
		t.Fatalf("Len=%d after drain, want 0", got)
	}
}

func TestQueue_Push_FullReturnsErrQueueFull(t *testing.T) {
	q := New[int](3)
	defer q.Close()

	if err := q.Push(1); err != nil {
		t.Fatalf("push 1: %v", err)
	}
	if err := q.Push(2); err != nil {
		t.Fatalf("push 2: %v", err)
	}
	if err := q.Push(3); err != nil {
		t.Fatalf("push 3: %v", err)
	}
	err := q.Push(4)
	if !errors.Is(err, queue.ErrQueueFull) {
		t.Fatalf("expected ErrQueueFull on full, got %v", err)
	}
	if got := q.Len(); got != 3 {
		t.Fatalf("Len=%d after full Push, want 3 (no partial insert)", got)
	}
}

func TestQueue_Pop_BlocksUntilPush(t *testing.T) {
	q := New[int](4)
	defer q.Close()

	popped := make(chan int, 1)
	popStarted := make(chan struct{})
	go func() {
		close(popStarted)
		v, err := q.Pop(context.Background())
		if err != nil {
			t.Errorf("pop: %v", err)
			return
		}
		popped <- v
	}()

	<-popStarted
	// Give the goroutine time to actually be waiting on the cond.
	// 10ms is enough to land on the Wait without making the test slow.
	time.Sleep(10 * time.Millisecond)
	select {
	case v := <-popped:
		t.Fatalf("Pop returned %d before Push happened — Pop did not block on empty queue", v)
	default:
	}

	if err := q.Push(42); err != nil {
		t.Fatalf("push: %v", err)
	}
	select {
	case v := <-popped:
		if v != 42 {
			t.Fatalf("popped=%d, want 42", v)
		}
	case <-time.After(time.Second):
		t.Fatalf("Pop did not unblock within 1s after Push")
	}
}

func TestQueue_Pop_RespectsContextCancel(t *testing.T) {
	q := New[int](2)
	defer q.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := q.Pop(ctx)
		done <- err
	}()

	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Pop did not unblock within 1s after ctx cancel")
	}
}

func TestQueue_Close_DrainsRemainingThenReturnsErrClosed(t *testing.T) {
	q := New[int](4)
	for i := 1; i <= 3; i++ {
		if err := q.Push(i); err != nil {
			t.Fatalf("push %d: %v", i, err)
		}
	}
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// Push after Close fails fast.
	if err := q.Push(99); !errors.Is(err, queue.ErrClosed) {
		t.Fatalf("push after close: expected ErrClosed, got %v", err)
	}
	// Remaining items pop in FIFO order.
	for i := 1; i <= 3; i++ {
		v, err := q.Pop(context.Background())
		if err != nil {
			t.Fatalf("drain pop %d: %v", i, err)
		}
		if v != i {
			t.Fatalf("drain pop %d: got %d, want %d", i, v, i)
		}
	}
	// Drained — Pop now returns ErrClosed.
	_, err := q.Pop(context.Background())
	if !errors.Is(err, queue.ErrClosed) {
		t.Fatalf("drained pop: expected ErrClosed, got %v", err)
	}
}

func TestQueue_Close_UnblocksPendingPop(t *testing.T) {
	q := New[int](2)

	done := make(chan error, 1)
	go func() {
		_, err := q.Pop(context.Background())
		done <- err
	}()

	time.Sleep(10 * time.Millisecond)
	if err := q.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, queue.ErrClosed) {
			t.Fatalf("expected ErrClosed after Close, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("Pop did not unblock within 1s after Close")
	}
}

func TestQueue_Close_Idempotent(t *testing.T) {
	q := New[int](1)
	if err := q.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := q.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// TestQueue_ConcurrentPushPop is the smoke test for the cond-var
// machinery: many producers and consumers, FIFO not required across
// goroutines (interleaved), but no items lost or duplicated.
func TestQueue_ConcurrentPushPop(t *testing.T) {
	const (
		producers = 8
		consumers = 8
		perProd   = 250
	)
	q := New[int](64)
	defer q.Close()

	var pushed, popped int64
	var wg sync.WaitGroup

	for c := 0; c < consumers; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
				_, err := q.Pop(ctx)
				cancel()
				if err != nil {
					return
				}
				atomic.AddInt64(&popped, 1)
			}
		}()
	}

	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perProd; i++ {
				for {
					if err := q.Push(i); err == nil {
						atomic.AddInt64(&pushed, 1)
						break
					}
					// On full, yield and retry.
					time.Sleep(time.Millisecond)
				}
			}
		}()
	}

	wg.Wait()
	if pushed != popped {
		t.Fatalf("pushed=%d popped=%d (lost or duplicated items)", pushed, popped)
	}
	expect := int64(producers * perProd)
	if pushed != expect {
		t.Fatalf("pushed=%d, want %d", pushed, expect)
	}
}

func TestQueue_PanicsOnNonPositiveCapacity(t *testing.T) {
	for _, capacity := range []int{0, -1, -100} {
		t.Run("", func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("New(%d) did not panic", capacity)
				}
			}()
			_ = New[int](capacity)
		})
	}
}
