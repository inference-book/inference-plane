package engines

import (
	"sync"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// memStore is an in-memory Store. Safe for concurrent use because the
// concurrency test drives N goroutines through it.
type memStore struct {
	mu sync.Mutex
	m  map[string]*provisionerv1.Engine
}

func newMemStore() *memStore { return &memStore{m: map[string]*provisionerv1.Engine{}} }

func (s *memStore) PutEngine(e *provisionerv1.Engine) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[e.GetId()] = proto.Clone(e).(*provisionerv1.Engine)
	return nil
}

func (s *memStore) ListEngines() ([]*provisionerv1.Engine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*provisionerv1.Engine, 0, len(s.m))
	for _, e := range s.m {
		out = append(out, proto.Clone(e).(*provisionerv1.Engine))
	}
	return out, nil
}

// fakeClock is a manually advanced time source so lease expiry is tested
// without sleeping.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)}
}
func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func engine(id string) *provisionerv1.Engine {
	return &provisionerv1.Engine{
		Id:       id,
		Model:    "test-model",
		Endpoint: "http://127.0.0.1:9001",
		State:    provisionerv1.EngineState_ENGINE_STATE_SERVING,
	}
}

func TestRegisterRequiresID(t *testing.T) {
	r := New(newMemStore())
	if _, err := r.Register(&provisionerv1.Engine{Model: "m"}); err == nil {
		t.Fatal("registration without an id was accepted; a renewal is indistinguishable from a new member without one")
	}
}

// An engine may not declare itself LOST. That state is the control plane's
// conclusion from an expired lease, and letting an engine assert it would let
// a caller remove itself from the fleet view while still serving traffic.
func TestRegisterRejectsSelfReportedLost(t *testing.T) {
	r := New(newMemStore())
	e := engine("e1")
	e.State = provisionerv1.EngineState_ENGINE_STATE_LOST
	if _, err := r.Register(e); err == nil {
		t.Fatal("engine was allowed to report itself LOST")
	}
}

func TestRegisterIsIdempotentOnID(t *testing.T) {
	clock := newClock()
	store := newMemStore()
	r := New(store, WithClock(clock.now))

	if _, err := r.Register(engine("e1")); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if _, err := r.Register(engine("e1")); err != nil {
		t.Fatal(err)
	}

	all, _ := r.List("", provisionerv1.EngineState_ENGINE_STATE_UNSPECIFIED)
	if len(all) != 1 {
		t.Fatalf("re-registration created %d members, want 1 (it is a renewal)", len(all))
	}
}

// registered_at survives renewal so an operator can see how long an engine
// has been up; last_seen_at and the lease move forward.
func TestRenewalPreservesRegisteredAt(t *testing.T) {
	clock := newClock()
	r := New(newMemStore(), WithClock(clock.now))

	first, err := r.Register(engine("e1"))
	if err != nil {
		t.Fatal(err)
	}
	firstRegistered := first.GetRegisteredAt().AsTime()

	clock.advance(10 * time.Second)
	second, err := r.Register(engine("e1"))
	if err != nil {
		t.Fatal(err)
	}

	if !second.GetRegisteredAt().AsTime().Equal(firstRegistered) {
		t.Errorf("registered_at moved on renewal: %v -> %v", firstRegistered, second.GetRegisteredAt().AsTime())
	}
	if !second.GetLastSeenAt().AsTime().After(first.GetLastSeenAt().AsTime()) {
		t.Error("last_seen_at did not advance on renewal")
	}
	if !second.GetLeaseExpiresAt().AsTime().After(first.GetLeaseExpiresAt().AsTime()) {
		t.Error("lease did not extend on renewal")
	}
}

// A caller must not be able to extend its own lease by claiming a later
// expiry; the control plane owns that field.
func TestRegisterIgnoresCallerSuppliedLease(t *testing.T) {
	clock := newClock()
	r := New(newMemStore(), WithClock(clock.now), WithLease(30*time.Second))

	e := engine("e1")
	e.LeaseExpiresAt = timestampAfter(clock.now(), 999*time.Hour)

	stored, err := r.Register(e)
	if err != nil {
		t.Fatal(err)
	}
	want := clock.now().Add(30 * time.Second)
	if !stored.GetLeaseExpiresAt().AsTime().Equal(want) {
		t.Errorf("lease = %v, want %v; the caller's claimed expiry was honoured",
			stored.GetLeaseExpiresAt().AsTime(), want)
	}
}

func TestExpireOverdueMarksLost(t *testing.T) {
	clock := newClock()
	r := New(newMemStore(), WithClock(clock.now), WithLease(30*time.Second))

	if _, err := r.Register(engine("e1")); err != nil {
		t.Fatal(err)
	}

	clock.advance(29 * time.Second)
	if n, _ := r.ExpireOverdue(); n != 0 {
		t.Fatalf("expired %d engines before the lease ran out", n)
	}

	clock.advance(2 * time.Second)
	n, err := r.ExpireOverdue()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expired %d engines, want 1", n)
	}
	all, _ := r.List("", provisionerv1.EngineState_ENGINE_STATE_UNSPECIFIED)
	if got := all[0].GetState(); got != provisionerv1.EngineState_ENGINE_STATE_LOST {
		t.Errorf("state = %v, want LOST", got)
	}
}

// Renewal before expiry keeps a member alive indefinitely. The loop that
// matters: an engine that keeps talking is never declared gone.
func TestRenewalPreventsExpiry(t *testing.T) {
	clock := newClock()
	r := New(newMemStore(), WithClock(clock.now), WithLease(30*time.Second))

	for range 10 {
		if _, err := r.Register(engine("e1")); err != nil {
			t.Fatal(err)
		}
		clock.advance(10 * time.Second)
		if n, _ := r.ExpireOverdue(); n != 0 {
			t.Fatalf("a renewing engine was expired")
		}
	}
}

// Sweeping repeatedly must not churn: an engine already LOST is left alone,
// so the state file is not rewritten every tick for a fleet with dead members.
func TestExpireOverdueIsIdempotent(t *testing.T) {
	clock := newClock()
	r := New(newMemStore(), WithClock(clock.now), WithLease(time.Second))

	if _, err := r.Register(engine("e1")); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Second)
	if n, _ := r.ExpireOverdue(); n != 1 {
		t.Fatal("first sweep did not expire the engine")
	}
	if n, _ := r.ExpireOverdue(); n != 0 {
		t.Errorf("second sweep changed %d engines, want 0", n)
	}
}

// A LOST engine that comes back is revived. The control plane's conclusion
// was a timeout, and the engine is the authority on whether it exists.
func TestLostEngineCanReRegister(t *testing.T) {
	clock := newClock()
	r := New(newMemStore(), WithClock(clock.now), WithLease(time.Second))

	if _, err := r.Register(engine("e1")); err != nil {
		t.Fatal(err)
	}
	clock.advance(2 * time.Second)
	if _, err := r.ExpireOverdue(); err != nil {
		t.Fatal(err)
	}

	revived, err := r.Register(engine("e1"))
	if err != nil {
		t.Fatalf("a LOST engine could not re-register: %v", err)
	}
	if revived.GetState() == provisionerv1.EngineState_ENGINE_STATE_LOST {
		t.Error("re-registration left the engine LOST")
	}
}

func TestRegisterDefaultsToAssembling(t *testing.T) {
	r := New(newMemStore())
	stored, err := r.Register(&provisionerv1.Engine{Id: "e1"})
	if err != nil {
		t.Fatal(err)
	}
	if stored.GetState() != provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING {
		t.Errorf("state = %v, want ASSEMBLING; an engine that has not said it is serving has not finished forming its group",
			stored.GetState())
	}
}

func TestSpanIsOrderedByNodeIndex(t *testing.T) {
	r := New(newMemStore())
	e := engine("e1")
	e.Span = []*provisionerv1.EngineNode{
		{HostId: "h2", NodeIndex: 2, GpuCount: 4},
		{HostId: "h0", NodeIndex: 0, GpuCount: 4},
		{HostId: "h1", NodeIndex: 1, GpuCount: 4},
	}
	stored, err := r.Register(e)
	if err != nil {
		t.Fatal(err)
	}
	for i, node := range stored.GetSpan() {
		if node.GetNodeIndex() != int32(i) {
			t.Fatalf("span[%d] has node_index %d; span is not ordered", i, node.GetNodeIndex())
		}
	}
	if got := TotalCards(stored); got != 12 {
		t.Errorf("TotalCards = %d, want 12", got)
	}
}

func TestListFilters(t *testing.T) {
	r := New(newMemStore())
	a := engine("a")
	a.DeploymentId = "dep1"
	b := engine("b")
	b.DeploymentId = "dep2"
	b.State = provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING
	for _, e := range []*provisionerv1.Engine{a, b} {
		if _, err := r.Register(e); err != nil {
			t.Fatal(err)
		}
	}

	byDeploy, _ := r.List("dep1", provisionerv1.EngineState_ENGINE_STATE_UNSPECIFIED)
	if len(byDeploy) != 1 || byDeploy[0].GetId() != "a" {
		t.Errorf("deployment filter returned %v", ids(byDeploy))
	}
	byState, _ := r.List("", provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING)
	if len(byState) != 1 || byState[0].GetId() != "b" {
		t.Errorf("state filter returned %v", ids(byState))
	}
	all, _ := r.List("", provisionerv1.EngineState_ENGINE_STATE_UNSPECIFIED)
	if len(all) != 2 {
		t.Errorf("unfiltered list returned %v", ids(all))
	}
}

// N agents renewing at the same instant is the normal case, not an edge one,
// and each renewal is a read-modify-write. The same lost-update shape bit
// multi-replica deploys whose per-slot endpoint patches landed together.
func TestConcurrentRegistrationsDoNotLoseMembers(t *testing.T) {
	r := New(newMemStore())
	const n = 32

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := r.Register(engine("engine-" + itoa(i))); err != nil {
				t.Errorf("register %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	all, err := r.List("", provisionerv1.EngineState_ENGINE_STATE_UNSPECIFIED)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Errorf("registered %d engines concurrently, registry holds %d", n, len(all))
	}
}

func ids(es []*provisionerv1.Engine) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.GetId()
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func timestampAfter(t time.Time, d time.Duration) *timestamppb.Timestamp {
	return timestamppb.New(t.Add(d))
}
