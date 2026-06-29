package scheduler

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// tenantEntry is a test Entry impl that carries a tenant id +
// stamp/read methods for the EnqueueTimestamper interface. Used by
// fair-share + observer tests.
type tenantEntry struct {
	seq      int
	deploy   string
	priority string
	tenant   string

	mu  sync.Mutex
	at  time.Time
}

func (e *tenantEntry) DeploymentID() string         { return e.deploy }
func (e *tenantEntry) Priority() string             { return e.priority }
func (e *tenantEntry) Tenant() string               { return e.tenant }
func (e *tenantEntry) StampEnqueued(t time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.at = t
}
func (e *tenantEntry) EnqueuedAt() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.at
}

// TestInteractiveFirst_WeightedFairShare_RatioUnderSaturation
// asserts the v0.2 ch7-beat2.5 acceptance: two tenants with weights
// 1:3 in the same lane see dispatch ratio close to 1:3 while both
// sub-queues are saturated.
//
// Technique: pre-populate both queues with WAY more items than
// the measurement window (sample size). Stop the scheduler after
// the target sample size is reached. The first N dispatches all
// happen with both sub-queues non-empty, so the lottery operates
// at full weight ratio throughout.
func TestInteractiveFirst_WeightedFairShare_RatioUnderSaturation(t *testing.T) {
	const (
		sampleSize        = 800 // large enough to bound lottery variance
		prepopulate       = 2000
		aliceWeight       = 1
		bobWeight         = 3
		expectedAliceFrac = float64(aliceWeight) / float64(aliceWeight+bobWeight)
		expectedBobFrac   = float64(bobWeight) / float64(aliceWeight+bobWeight)
		tolerance         = 0.05 // ±5% absolute on the ratio
	)

	var aliceCount, bobCount, totalDispatched int64
	allDispatched := make(chan struct{})
	var once sync.Once

	s := NewInteractiveFirst(InteractiveFirstConfig{
		Workers:             1,
		InteractiveCapacity: prepopulate + 1,
		BatchCapacity:       1,
		Weights:             Weights{"alice": aliceWeight, "bob": bobWeight},
		Handler: func(_ context.Context, e Entry) {
			te := e.(*tenantEntry)
			switch te.tenant {
			case "alice":
				atomic.AddInt64(&aliceCount, 1)
			case "bob":
				atomic.AddInt64(&bobCount, 1)
			}
			if atomic.AddInt64(&totalDispatched, 1) >= sampleSize {
				once.Do(func() { close(allDispatched) })
			}
		},
	})

	// Pre-populate well above the sample size so both sub-queues
	// remain non-empty through the entire measurement window.
	for i := 0; i < prepopulate; i++ {
		if err := s.Submit(&tenantEntry{seq: i, deploy: "d1", priority: LaneInteractive, tenant: "alice"}); err != nil {
			t.Fatalf("alice submit %d: %v", i, err)
		}
		if err := s.Submit(&tenantEntry{seq: i, deploy: "d1", priority: LaneInteractive, tenant: "bob"}); err != nil {
			t.Fatalf("bob submit %d: %v", i, err)
		}
	}

	s.Start(context.Background())

	select {
	case <-allDispatched:
	case <-time.After(5 * time.Second):
		t.Fatalf("never reached sample size %d (got %d)", sampleSize, atomic.LoadInt64(&totalDispatched))
	}

	// Stop the scheduler so further dispatches don't pollute the
	// counts. Some extra dispatches may slip in between the close
	// and Stop returning; tolerance covers that slack.
	s.Stop()

	alice := atomic.LoadInt64(&aliceCount)
	bob := atomic.LoadInt64(&bobCount)
	total := float64(alice + bob)
	aliceFrac := float64(alice) / total
	bobFrac := float64(bob) / total

	if math.Abs(aliceFrac-expectedAliceFrac) > tolerance {
		t.Errorf("alice fraction = %.3f (count=%d / %d), expected ~%.3f ± %.3f",
			aliceFrac, alice, int64(total), expectedAliceFrac, tolerance)
	}
	if math.Abs(bobFrac-expectedBobFrac) > tolerance {
		t.Errorf("bob fraction = %.3f (count=%d / %d), expected ~%.3f ± %.3f",
			bobFrac, bob, int64(total), expectedBobFrac, tolerance)
	}
}

// TestInteractiveFirst_UnknownTenant_GetsDefaultWeight asserts a
// tenant not in the Weights config still gets dispatched (no
// silent drop) at the default weight.
func TestInteractiveFirst_UnknownTenant_GetsDefaultWeight(t *testing.T) {
	got := make(chan string, 3)
	s := NewInteractiveFirst(InteractiveFirstConfig{
		Workers:             1,
		InteractiveCapacity: 4,
		BatchCapacity:       1,
		Weights:             Weights{"alice": 1}, // bob NOT listed
		Handler: func(_ context.Context, e Entry) {
			got <- e.(*tenantEntry).tenant
		},
	})

	_ = s.Submit(&tenantEntry{seq: 1, deploy: "d", priority: LaneInteractive, tenant: "alice"})
	_ = s.Submit(&tenantEntry{seq: 2, deploy: "d", priority: LaneInteractive, tenant: "bob"})
	_ = s.Submit(&tenantEntry{seq: 3, deploy: "d", priority: LaneInteractive, tenant: "carol"})

	s.Start(context.Background())
	defer s.Stop()

	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		select {
		case t := <-got:
			seen[t] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only got %d/3 dispatches: %v", i, seen)
		}
	}
	for _, tenant := range []string{"alice", "bob", "carol"} {
		if !seen[tenant] {
			t.Errorf("tenant %q never dispatched (default-weight tenants should still flow)", tenant)
		}
	}
}

// TestInteractiveFirst_PerTenantSubQueue_Isolation: one tenant's
// sub-queue filling up doesn't reject another tenant's submits.
// Capacity is per-tenant, not per-lane shared.
func TestInteractiveFirst_PerTenantSubQueue_Isolation(t *testing.T) {
	const perTenantCap = 2
	hold := make(chan struct{})
	s := NewInteractiveFirst(InteractiveFirstConfig{
		Workers:             0 + 1, // 1 worker; held below so queues fill
		InteractiveCapacity: perTenantCap,
		BatchCapacity:       perTenantCap,
		Handler: func(_ context.Context, _ Entry) {
			<-hold
		},
	})
	s.Start(context.Background())
	defer func() {
		close(hold)
		s.Stop()
	}()

	// Fill alice's sub-queue to capacity. The 1 worker pops first
	// item and holds; remaining items occupy the queue.
	for i := 0; i < perTenantCap+1; i++ {
		_ = s.Submit(&tenantEntry{seq: i, deploy: "d", priority: LaneInteractive, tenant: "alice"})
	}

	// Bob's first submit should still succeed -- his sub-queue is
	// independent of alice's. Give the scheduler a moment to settle.
	time.Sleep(20 * time.Millisecond)
	if err := s.Submit(&tenantEntry{seq: 99, deploy: "d", priority: LaneInteractive, tenant: "bob"}); err != nil {
		t.Errorf("bob submit returned %v; want nil (per-tenant capacity isolation broken)", err)
	}
}

// recordingObserver captures push/pop calls for the observer tests.
type recordingObserver struct {
	mu     sync.Mutex
	pushes []pushEvent
	pops   []popEvent
}

type pushEvent struct {
	lane, tenant string
	depth        int
}

type popEvent struct {
	lane, tenant, deploy string
	depth                int
	waitNonZero          bool
}

func (o *recordingObserver) OnPush(lane, tenant string, depth int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pushes = append(o.pushes, pushEvent{lane, tenant, depth})
}

func (o *recordingObserver) OnPop(lane, tenant, deploy string, depth int, wait time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pops = append(o.pops, popEvent{lane, tenant, deploy, depth, wait > 0})
}

// TestInteractiveFirst_Observer_FiresOnPushAndPop verifies the
// scheduler invokes the Observer hooks with the right labels at
// the right times. This is the #80 wiring: the metrics package
// hangs off these hooks.
func TestInteractiveFirst_Observer_FiresOnPushAndPop(t *testing.T) {
	obs := &recordingObserver{}
	done := make(chan struct{})
	s := NewInteractiveFirst(InteractiveFirstConfig{
		Workers:             1,
		InteractiveCapacity: 4,
		BatchCapacity:       1,
		Weights:             Weights{"alice": 1},
		Observer:            obs,
		Handler: func(_ context.Context, _ Entry) {
			done <- struct{}{}
		},
	})
	s.Start(context.Background())
	defer s.Stop()

	_ = s.Submit(&tenantEntry{seq: 1, deploy: "my-llama", priority: LaneInteractive, tenant: "alice"})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler never fired")
	}

	obs.mu.Lock()
	defer obs.mu.Unlock()
	if len(obs.pushes) != 1 {
		t.Fatalf("OnPush calls = %d, want 1", len(obs.pushes))
	}
	if got := obs.pushes[0]; got.lane != LaneInteractive || got.tenant != "alice" || got.depth != 1 {
		t.Errorf("OnPush event = %+v, want {interactive, alice, 1}", got)
	}
	if len(obs.pops) != 1 {
		t.Fatalf("OnPop calls = %d, want 1", len(obs.pops))
	}
	if got := obs.pops[0]; got.lane != LaneInteractive || got.tenant != "alice" || got.deploy != "my-llama" {
		t.Errorf("OnPop event = %+v, want {interactive, alice, my-llama}", got)
	}
	if !obs.pops[0].waitNonZero {
		t.Errorf("OnPop wait duration is zero; expected nonzero from StampEnqueued path")
	}
}
