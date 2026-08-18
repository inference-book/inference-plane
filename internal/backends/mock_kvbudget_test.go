package backends

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKVBudgetAdmitsUpToItsCapacity(t *testing.T) {
	b := newKVBudget(100)
	for range 4 {
		if err := b.acquire(context.Background(), 25); err != nil {
			t.Fatal(err)
		}
	}
	if b.used != 100 {
		t.Errorf("used = %d, want 100", b.used)
	}
}

// The gate is weighted, so what it admits depends on how long the
// sequences are and not only on how many there are. That is the whole
// reason it exists rather than a plain request counter.
func TestKVBudgetAdmitsFewerLongSequencesThanShortOnes(t *testing.T) {
	// Deterministic in both directions: the admissions that should
	// succeed are taken against an uncancellable context so a loaded test
	// runner cannot turn a slow schedule into a false rejection, and only
	// the one that should block waits on a clock.
	for _, tc := range []struct{ cost, want int }{
		{100, 10},
		{500, 2},
		{1000, 1},
	} {
		b := newKVBudget(1000)
		for i := range tc.want {
			if err := b.acquire(context.Background(), tc.cost); err != nil {
				t.Fatalf("sequences costing %d: refused number %d of an expected %d: %v", tc.cost, i+1, tc.want, err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		err := b.acquire(ctx, tc.cost)
		cancel()
		if err == nil {
			t.Errorf("sequences costing %d: admitted %d, one more than the pool holds", tc.cost, tc.want+1)
		}
	}
}

func TestKVBudgetReleasesRoomForTheNextSequence(t *testing.T) {
	b := newKVBudget(100)
	if err := b.acquire(context.Background(), 100); err != nil {
		t.Fatal(err)
	}

	admitted := make(chan struct{})
	go func() {
		if err := b.acquire(context.Background(), 60); err == nil {
			close(admitted)
		}
	}()

	select {
	case <-admitted:
		t.Fatal("admitted a sequence into a full pool")
	case <-time.After(30 * time.Millisecond):
	}

	b.release(100)
	select {
	case <-admitted:
	case <-time.After(time.Second):
		t.Fatal("did not admit after the pool drained")
	}
}

// No amount of waiting frees space that does not exist, so a sequence
// larger than the whole pool is refused rather than parked forever. A
// real engine answers the same way to a sequence past its configured
// maximum.
func TestKVBudgetRefusesASequenceLargerThanItself(t *testing.T) {
	err := newKVBudget(100).acquire(context.Background(), 101)
	if err == nil {
		t.Fatal("parked a sequence that can never fit")
	}
	for _, want := range []string{"101", "100"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s; the operator has to change one of the two numbers", err, want)
		}
	}
}

// sync.Cond has no context-aware wait, so cancellation has to be
// broadcast explicitly. Without it a request waiting on a full pool
// ignores its deadline and the load driver's shutdown hangs behind the
// slowest occupant.
func TestKVBudgetWakesAWaiterWhoseContextIsCancelled(t *testing.T) {
	b := newKVBudget(100)
	if err := b.acquire(context.Background(), 100); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.acquire(ctx, 50) }()

	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("returned success for a cancelled caller")
		}
	case <-time.After(time.Second):
		t.Fatal("a cancelled waiter was never woken")
	}
}

// End to end through the backend: with a budget, how many requests are
// served at once falls as the prompts get longer. Without one it does
// not, which is the behaviour every demo built before capacity was a
// question depends on.
func TestMockConcurrencyCeilingFallsAsPromptsGrow(t *testing.T) {
	peakFor := func(promptChars, budget int) int64 {
		opts := []MockOption{WithLatency(20*time.Millisecond, 20*time.Millisecond), WithOutputTokens(10, 10)}
		if budget > 0 {
			opts = append(opts, WithKVBudget(budget))
		}
		m := NewMock("t", opts...)

		var inFlight, peak atomic.Int64
		var wg sync.WaitGroup
		for range 32 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := GenerateRequest{
					Messages:  []ChatMessage{{Role: "user", Content: strings.Repeat("a", promptChars)}},
					MaxTokens: 10,
				}
				n := inFlight.Add(1)
				for {
					p := peak.Load()
					if n <= p || peak.CompareAndSwap(p, n) {
						break
					}
				}
				_, _ = m.Generate(context.Background(), req)
				inFlight.Add(-1)
			}()
		}
		wg.Wait()
		return peak.Load()
	}

	// Without a budget the engine admits whatever arrives.
	if got := peakFor(4000, 0); got < 30 {
		t.Errorf("unbudgeted engine peaked at %d of 32 concurrent", got)
	}

	// The measurement is of what the gate admits rather than of what the
	// client offered, so it is taken from the gate's own accounting.
	admitted := func(promptChars, budget int) int {
		m := NewMock("t", WithKVBudget(budget), WithOutputTokens(10, 10))
		cost := approximateTokens(GenerateRequest{
			Messages: []ChatMessage{{Role: "user", Content: strings.Repeat("a", promptChars)}},
		}) + 10
		// Arithmetic rather than a clock, for the reason the weighted
		// test above gives. What the gate admits is the pool divided by
		// the cost, and the assertion is that the gate agrees.
		want := budget / cost
		for i := range want {
			if err := m.kv.acquire(context.Background(), cost); err != nil {
				t.Fatalf("prompt of %d chars: refused number %d of an expected %d: %v", promptChars, i+1, want, err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		defer cancel()
		if err := m.kv.acquire(ctx, cost); err == nil {
			t.Errorf("prompt of %d chars: admitted one more than the %d the pool holds", promptChars, want)
		}
		return want
	}
	short := admitted(400, 20_000)
	long := admitted(8000, 20_000)
	if short <= long {
		t.Errorf("100-token prompts admitted %d and 2000-token prompts admitted %d; the ceiling did not fall", short, long)
	}
}
