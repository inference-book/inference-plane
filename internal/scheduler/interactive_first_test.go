package scheduler

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/inference-book/inference-plane/internal/stores/queue"
)

// fakeEntry is the test-side Entry impl. Carries a sequence number
// so tests can assert FIFO order, a deployment id for the in-flight
// bucket, and a priority label.
type fakeEntry struct {
	seq      int
	deploy   string
	priority string
	done     chan struct{} // closed by the handler when work finishes
}

func (e *fakeEntry) DeploymentID() string { return e.deploy }
func (e *fakeEntry) Priority() string     { return e.priority }

// newScheduler builds a default-ish InteractiveFirst with the
// supplied handler. Tests vary only the params they care about.
func newScheduler(t *testing.T, workers, cap int, handler HandlerFunc) *InteractiveFirst {
	t.Helper()
	s := NewInteractiveFirst(InteractiveFirstConfig{
		Workers:             workers,
		InteractiveCapacity: 16,
		BatchCapacity:       16,
		InFlightCap:         cap,
		Handler:             handler,
	})
	return s
}

func TestInteractiveFirst_FIFOWithinLane(t *testing.T) {
	var mu sync.Mutex
	var got []int
	s := newScheduler(t, 1, 0, func(_ context.Context, e Entry) {
		mu.Lock()
		got = append(got, e.(*fakeEntry).seq)
		mu.Unlock()
	})
	s.Start(context.Background())
	defer s.Stop()

	for i := 0; i < 8; i++ {
		if err := s.Submit(&fakeEntry{seq: i, deploy: "d1", priority: LaneInteractive}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	if !waitForLen(func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	}, 8) {
		t.Fatalf("never drained 8 items: got %d", len(got))
	}
	mu.Lock()
	defer mu.Unlock()
	for i, v := range got {
		if v != i {
			t.Errorf("got[%d]=%d, want %d (FIFO within interactive lane violated)", i, v, i)
		}
	}
}

// TestInteractiveFirst_StrictPriority_FirstDispatchIsInteractive
// asserts deterministic strict priority: with both lanes
// pre-populated before Start, the worker's first round MUST dispatch
// an interactive entry (the higher-priority queue gets directly
// checked first via tryPop).
func TestInteractiveFirst_StrictPriority_FirstDispatchIsInteractive(t *testing.T) {
	type result struct {
		priority string
		seq      int
	}
	var mu sync.Mutex
	var observed []result

	startGate := make(chan struct{})
	s := newScheduler(t, 1, 0, func(_ context.Context, e Entry) {
		<-startGate
		fe := e.(*fakeEntry)
		mu.Lock()
		observed = append(observed, result{priority: fe.priority, seq: fe.seq})
		mu.Unlock()
	})

	// Pre-populate batch FIRST, then interactive. With the new
	// direct-queue-inspection worker loop, the worker's first round
	// tryPops interactive (which now has an item) BEFORE batch -- so
	// the first dispatch is deterministically interactive.
	for i := 0; i < 3; i++ {
		if err := s.Submit(&fakeEntry{seq: i, deploy: "d1", priority: LaneBatch}); err != nil {
			t.Fatalf("batch submit %d: %v", i, err)
		}
	}
	if err := s.Submit(&fakeEntry{seq: 100, deploy: "d1", priority: LaneInteractive}); err != nil {
		t.Fatalf("interactive submit: %v", err)
	}

	s.Start(context.Background())
	defer s.Stop()
	close(startGate)

	if !waitForLen(func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(observed)
	}, 4) {
		t.Fatalf("never drained 4 items: got %d", len(observed))
	}
	mu.Lock()
	defer mu.Unlock()
	if observed[0].priority != LaneInteractive {
		t.Errorf("first dispatched priority=%q seq=%d, want interactive (strict priority violated)",
			observed[0].priority, observed[0].seq)
	}
}

// TestInteractiveFirst_AggregatePriority_InteractiveDominatesUnderLoad
// asserts that under sustained mixed traffic, interactive entries
// dispatch before batch entries in aggregate. With strict priority
// (direct queue inspection) all interactive entries should complete
// before batch starts -- the worker only checks batch when
// interactive is empty.
func TestInteractiveFirst_AggregatePriority_InteractiveDominatesUnderLoad(t *testing.T) {
	const eachLane = 8

	type result struct {
		priority string
		round    int
	}
	var mu sync.Mutex
	var observed []result
	round := 0

	s := newScheduler(t, 1, 0, func(_ context.Context, e Entry) {
		fe := e.(*fakeEntry)
		mu.Lock()
		round++
		observed = append(observed, result{priority: fe.priority, round: round})
		mu.Unlock()
	})

	// Pre-populate both lanes before Start so the scheduler sees a
	// mix from the first dispatch tick.
	for i := 0; i < eachLane; i++ {
		if err := s.Submit(&fakeEntry{seq: i, deploy: "d1", priority: LaneBatch}); err != nil {
			t.Fatalf("batch submit %d: %v", i, err)
		}
		if err := s.Submit(&fakeEntry{seq: 100 + i, deploy: "d1", priority: LaneInteractive}); err != nil {
			t.Fatalf("interactive submit %d: %v", i, err)
		}
	}

	s.Start(context.Background())
	defer s.Stop()

	if !waitForLen(func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(observed)
	}, 2*eachLane) {
		t.Fatalf("never drained %d items: got %d", 2*eachLane, len(observed))
	}

	mu.Lock()
	defer mu.Unlock()
	// Average dispatch round for each lane. Interactive should have
	// a substantially lower average round than batch -- the chapter
	// narrative ("interactive cuts ahead") holds in aggregate even
	// though per-request ordering is racy under contention.
	var interactiveSum, batchSum, interactiveCount, batchCount int
	for _, r := range observed {
		if r.priority == LaneInteractive {
			interactiveSum += r.round
			interactiveCount++
		} else {
			batchSum += r.round
			batchCount++
		}
	}
	if interactiveCount != eachLane || batchCount != eachLane {
		t.Fatalf("dispatch counts: interactive=%d batch=%d, want %d each", interactiveCount, batchCount, eachLane)
	}
	interactiveAvg := float64(interactiveSum) / float64(interactiveCount)
	batchAvg := float64(batchSum) / float64(batchCount)
	if interactiveAvg >= batchAvg {
		t.Errorf("interactive avg dispatch round=%.1f, batch avg=%.1f; interactive should be lower (strict priority violated)",
			interactiveAvg, batchAvg)
	}
	// Strict priority: ALL interactive should dispatch before ANY
	// batch when both lanes are pre-populated and the worker is
	// always tryPopping interactive first.
	for i := 0; i < eachLane; i++ {
		if observed[i].priority != LaneInteractive {
			t.Errorf("observed[%d].priority=%q, want interactive (first %d dispatches should all be interactive)",
				i, observed[i].priority, eachLane)
			break
		}
	}
}

// Strict-priority enforcement at the per-request level is NOT
// guaranteed when an interactive entry arrives DURING a batch
// dispatch. The feeder-channel design means the worker's phase-1
// non-blocking try fires against the channel, not the queue
// directly; if the interactive feeder hasn't yet pushed the new
// item to the channel when the worker enters phase 2, the worker
// can pick a buffered batch item one round before the interactive
// one. Aggregate behavior is still strict-priority (interactive
// p95 stays bounded while batch backlogs grow); per-request
// ordering under contention is best-effort.
//
// Demo 05 (#82) is the empirical acceptance for the aggregate
// property. The book chapter explicitly teaches the bulk-priority
// guarantee, not per-request ordering.

func TestInteractiveFirst_InFlightCap_EnforcedPerDeployment(t *testing.T) {
	const cap = 2
	hold := make(chan struct{})
	var inFlight, peak atomic.Int32

	s := newScheduler(t, 8, cap, func(_ context.Context, e Entry) {
		current := inFlight.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		<-hold
		inFlight.Add(-1)
	})
	s.Start(context.Background())
	defer func() {
		close(hold)
		s.Stop()
	}()

	// Submit 8 entries all targeting the same deployment.
	for i := 0; i < 8; i++ {
		if err := s.Submit(&fakeEntry{seq: i, deploy: "d1", priority: LaneInteractive}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}

	// Wait until the cap is hit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if inFlight.Load() == int32(cap) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := inFlight.Load(); got != int32(cap) {
		t.Fatalf("inFlight=%d, want %d (cap not enforced)", got, cap)
	}
	// Give it more time to confirm the cap is sticky.
	time.Sleep(30 * time.Millisecond)
	if got := inFlight.Load(); got > int32(cap) {
		t.Fatalf("inFlight grew past %d to %d (cap broken)", cap, got)
	}
	if got := peak.Load(); got != int32(cap) {
		t.Errorf("peak inFlight=%d, want %d", got, cap)
	}
}

// TestInteractiveFirst_InFlightCap_PerDeploymentBucket: cap is 1
// per deployment; with 2 deployments, expect up to 2 in flight
// total (one per deployment), not 1 globally.
func TestInteractiveFirst_InFlightCap_PerDeploymentBucket(t *testing.T) {
	hold := make(chan struct{})
	var inFlight, peak atomic.Int32

	s := newScheduler(t, 4, 1, func(_ context.Context, e Entry) {
		current := inFlight.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		<-hold
		inFlight.Add(-1)
	})
	s.Start(context.Background())
	defer func() {
		close(hold)
		s.Stop()
	}()

	// One submit per deployment.
	_ = s.Submit(&fakeEntry{seq: 1, deploy: "d1", priority: LaneInteractive})
	_ = s.Submit(&fakeEntry{seq: 2, deploy: "d2", priority: LaneInteractive})
	// And another to each so the queue isn't empty when the first one's done.
	_ = s.Submit(&fakeEntry{seq: 3, deploy: "d1", priority: LaneInteractive})
	_ = s.Submit(&fakeEntry{seq: 4, deploy: "d2", priority: LaneInteractive})

	// Wait until 2 are in flight (one per deployment, both bypassing
	// their per-deployment cap of 1).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if inFlight.Load() == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := inFlight.Load(); got != 2 {
		t.Fatalf("inFlight=%d, want 2 (per-deployment cap means 1 of each)", got)
	}
	// Hold sticky.
	time.Sleep(30 * time.Millisecond)
	if got := inFlight.Load(); got > 2 {
		t.Fatalf("inFlight grew past 2 to %d (per-deployment cap broken)", got)
	}
}

func TestInteractiveFirst_Submit_FullReturnsErrQueueFull(t *testing.T) {
	s := NewInteractiveFirst(InteractiveFirstConfig{
		Workers:             1,
		InteractiveCapacity: 2,
		BatchCapacity:       2,
		Handler: func(_ context.Context, _ Entry) {
			time.Sleep(10 * time.Second) // never finishes during test
		},
	})
	// Don't Start so nothing drains. Fill the interactive lane.
	for i := 0; i < 2; i++ {
		if err := s.Submit(&fakeEntry{seq: i, deploy: "d", priority: LaneInteractive}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	err := s.Submit(&fakeEntry{seq: 999, deploy: "d", priority: LaneInteractive})
	if !errors.Is(err, queue.ErrQueueFull) {
		t.Errorf("submit on full interactive: got %v, want queue.ErrQueueFull", err)
	}
}

func TestInteractiveFirst_Stop_Idempotent(t *testing.T) {
	s := newScheduler(t, 1, 0, func(_ context.Context, _ Entry) {})
	s.Start(context.Background())
	s.Stop()
	s.Stop() // must not panic / hang
}

func TestInteractiveFirst_Stop_BeforeStartIsNoOp(t *testing.T) {
	s := newScheduler(t, 1, 0, func(_ context.Context, _ Entry) {})
	s.Stop() // must not hang
}

func TestInteractiveFirst_Len(t *testing.T) {
	s := NewInteractiveFirst(InteractiveFirstConfig{
		Workers:             1,
		InteractiveCapacity: 4,
		BatchCapacity:       4,
		Handler: func(_ context.Context, _ Entry) {
			time.Sleep(10 * time.Second) // never finishes during test
		},
	})
	// No Start -> Len reads the raw queue.
	if got := s.Len(LaneInteractive); got != 0 {
		t.Errorf("initial Len(interactive)=%d, want 0", got)
	}
	_ = s.Submit(&fakeEntry{seq: 1, deploy: "d", priority: LaneInteractive})
	_ = s.Submit(&fakeEntry{seq: 2, deploy: "d", priority: LaneBatch})
	if got := s.Len(LaneInteractive); got != 1 {
		t.Errorf("Len(interactive)=%d, want 1", got)
	}
	if got := s.Len(LaneBatch); got != 1 {
		t.Errorf("Len(batch)=%d, want 1", got)
	}
	if got := s.Len("background"); got != -1 {
		t.Errorf("Len(unknown)=%d, want -1", got)
	}
}

// waitForLen polls a counter func until it reaches want or the
// poll budget expires (~2s). Returns true on success.
func waitForLen(counter func() int, want int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if counter() >= want {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

func TestNewInteractiveFirst_PanicsOnInvalidArgs(t *testing.T) {
	cases := []struct {
		name string
		cfg  InteractiveFirstConfig
	}{
		{"nil handler", InteractiveFirstConfig{Workers: 1, InteractiveCapacity: 1, BatchCapacity: 1}},
		{"zero workers", InteractiveFirstConfig{InteractiveCapacity: 1, BatchCapacity: 1, Handler: func(_ context.Context, _ Entry) {}}},
		{"zero interactive cap", InteractiveFirstConfig{Workers: 1, BatchCapacity: 1, Handler: func(_ context.Context, _ Entry) {}}},
		{"zero batch cap", InteractiveFirstConfig{Workers: 1, InteractiveCapacity: 1, Handler: func(_ context.Context, _ Entry) {}}},
		{"negative inflight", InteractiveFirstConfig{Workers: 1, InteractiveCapacity: 1, BatchCapacity: 1, InFlightCap: -1, Handler: func(_ context.Context, _ Entry) {}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("NewInteractiveFirst(%s) did not panic", tc.name)
				}
			}()
			_ = NewInteractiveFirst(tc.cfg)
		})
	}
}
