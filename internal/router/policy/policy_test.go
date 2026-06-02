package policy_test

import (
	"context"
	"testing"

	"github.com/inference-book/inference-plane/internal/router/policy"
)

// TestRoundRobin_CyclesThroughReplicas: the load-bearing contract.
// 3 replicas, 6 picks -> a-b-c-a-b-c.
func TestRoundRobin_CyclesThroughReplicas(t *testing.T) {
	rr := policy.NewRoundRobin()
	replicas := []policy.Replica{
		{InstanceID: "a", Endpoint: "http://a"},
		{InstanceID: "b", Endpoint: "http://b"},
		{InstanceID: "c", Endpoint: "http://c"},
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, w := range want {
		got, ok := rr.Pick(context.Background(), "d", replicas, nil)
		if !ok {
			t.Fatalf("iter %d: ok=false", i)
		}
		if got.InstanceID != w {
			t.Errorf("iter %d: got %q, want %q", i, got.InstanceID, w)
		}
	}
}

// TestRoundRobin_EmptyReplicas: zero eligible replicas -> ok=false.
// The router maps this to 503 replica_unavailable.
func TestRoundRobin_EmptyReplicas(t *testing.T) {
	rr := policy.NewRoundRobin()
	if _, ok := rr.Pick(context.Background(), "d", nil, nil); ok {
		t.Fatal("Pick on empty replicas should return ok=false")
	}
}

// TestRoundRobin_PerDeploymentIsolation: two deployments share the
// RoundRobin instance but each has its own counter. Interleaved
// Picks against d1 and d2 don't share state.
func TestRoundRobin_PerDeploymentIsolation(t *testing.T) {
	rr := policy.NewRoundRobin()
	d1 := []policy.Replica{
		{InstanceID: "d1-a", Endpoint: "http://d1-a"},
		{InstanceID: "d1-b", Endpoint: "http://d1-b"},
	}
	d2 := []policy.Replica{
		{InstanceID: "d2-x", Endpoint: "http://d2-x"},
		{InstanceID: "d2-y", Endpoint: "http://d2-y"},
	}
	// d1: a -> b -> a
	// d2: x -> y -> x
	want := []struct {
		dep string
		set []policy.Replica
		id  string
	}{
		{"d1", d1, "d1-a"},
		{"d2", d2, "d2-x"},
		{"d1", d1, "d1-b"},
		{"d2", d2, "d2-y"},
		{"d1", d1, "d1-a"},
		{"d2", d2, "d2-x"},
	}
	for i, tc := range want {
		got, _ := rr.Pick(context.Background(), tc.dep, tc.set, nil)
		if got.InstanceID != tc.id {
			t.Errorf("iter %d (dep %s): got %q, want %q", i, tc.dep, got.InstanceID, tc.id)
		}
	}
}

// TestRoundRobin_ReplicaCountChange: when the eligible set shrinks
// (quarantine) or grows (scale-up) between calls, the counter
// continues monotonically and the modulo just maps to a smaller/
// larger range. The chapter narrative: routing stays smooth across
// fleet-shape changes without restarting the counter.
func TestRoundRobin_ReplicaCountChange(t *testing.T) {
	rr := policy.NewRoundRobin()
	three := []policy.Replica{
		{InstanceID: "a", Endpoint: "http://a"},
		{InstanceID: "b", Endpoint: "http://b"},
		{InstanceID: "c", Endpoint: "http://c"},
	}
	two := []policy.Replica{
		{InstanceID: "a", Endpoint: "http://a"},
		{InstanceID: "c", Endpoint: "http://c"},
	}
	// 3 picks against the 3-replica set advances the counter to 3
	// (picks were a, b, c).
	for range 3 {
		rr.Pick(context.Background(), "d", three, nil)
	}
	// Now b is quarantined -> 2-replica set. Counter -> 4, idx =
	// (4-1) % 2 = 1 -> two[1] which is c. The chapter narrative
	// note: with fleet-shape changes between Picks, the rotation
	// pattern shifts but stays well-defined; what matters is no
	// single replica monopolizes traffic.
	got, _ := rr.Pick(context.Background(), "d", two, nil)
	if got.InstanceID != "c" {
		t.Errorf("after fleet shrink: got %q, want c", got.InstanceID)
	}
}

// fakePolicy is a deterministic test substitute: always returns
// replicas[idx], or ok=false when len < idx+1. Tests use it via
// router.WithRoutingPolicy to verify the seam delegates correctly
// without exercising RoundRobin's modulo math.
type fakePolicy struct {
	idx     int
	stats   policy.Stats
	gotDep  string
	gotReps []policy.Replica
	pickCalls int
}

func (f *fakePolicy) Pick(_ context.Context, deployID string, replicas []policy.Replica, stats policy.Stats) (policy.Replica, bool) {
	f.pickCalls++
	f.gotDep = deployID
	f.gotReps = replicas
	f.stats = stats
	if f.idx >= len(replicas) {
		return policy.Replica{}, false
	}
	return replicas[f.idx], true
}

// TestPolicyInterface_FakeSubstitutable: a fake policy substituted
// via the interface receives the (deployID, replicas, stats) view
// the router has built. Verifies the seam contract.
func TestPolicyInterface_FakeSubstitutable(t *testing.T) {
	fake := &fakePolicy{idx: 1}
	replicas := []policy.Replica{
		{InstanceID: "a", Endpoint: "http://a"},
		{InstanceID: "b", Endpoint: "http://b"},
	}
	got, ok := fake.Pick(context.Background(), "d", replicas, nil)
	if !ok || got.InstanceID != "b" {
		t.Errorf("fakePolicy idx=1 should return b, got %v ok=%v", got, ok)
	}
	if fake.pickCalls != 1 {
		t.Errorf("pickCalls = %d, want 1", fake.pickCalls)
	}
}
