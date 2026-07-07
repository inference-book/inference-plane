package policy_test

import (
	"context"
	"testing"

	"github.com/inference-book/inference-plane/internal/router/policy"
)

func affinityReplicas() []policy.Replica {
	return []policy.Replica{
		{InstanceID: "a", Endpoint: "http://a"},
		{InstanceID: "b", Endpoint: "http://b"},
		{InstanceID: "c", Endpoint: "http://c"},
	}
}

// TestPrefixAffinity_SticksSessionToReplica: the load-bearing contract.
// Repeated picks for one session return the same replica, where
// round-robin would rotate a-b-c.
func TestPrefixAffinity_SticksSessionToReplica(t *testing.T) {
	pa := policy.NewPrefixAffinity(0)
	reps := affinityReplicas()
	ctx := policy.WithSession(context.Background(), "sess-1")

	first, ok := pa.Pick(ctx, "d", reps, nil)
	if !ok {
		t.Fatal("first Pick: ok=false")
	}
	for i := range 5 {
		got, ok := pa.Pick(ctx, "d", reps, nil)
		if !ok {
			t.Fatalf("iter %d: ok=false", i)
		}
		if got.InstanceID != first.InstanceID {
			t.Errorf("iter %d: got %q, want sticky %q", i, got.InstanceID, first.InstanceID)
		}
	}
}

// TestPrefixAffinity_DistinctSessionsSpread: new sessions are placed via
// the round-robin fallback, so two fresh sessions land on different
// replicas rather than all piling onto one.
func TestPrefixAffinity_DistinctSessionsSpread(t *testing.T) {
	pa := policy.NewPrefixAffinity(0)
	reps := affinityReplicas()

	a, _ := pa.Pick(policy.WithSession(context.Background(), "sess-A"), "d", reps, nil)
	b, _ := pa.Pick(policy.WithSession(context.Background(), "sess-B"), "d", reps, nil)
	if a.InstanceID == b.InstanceID {
		t.Errorf("distinct sessions both placed on %q; expected spread", a.InstanceID)
	}
}

// TestPrefixAffinity_NoSessionFallsBackToRoundRobin: with no session key
// on the context, Pick cycles like plain round-robin.
func TestPrefixAffinity_NoSessionFallsBackToRoundRobin(t *testing.T) {
	pa := policy.NewPrefixAffinity(0)
	reps := affinityReplicas()
	want := []string{"a", "b", "c", "a"}
	for i, w := range want {
		got, ok := pa.Pick(context.Background(), "d", reps, nil)
		if !ok {
			t.Fatalf("iter %d: ok=false", i)
		}
		if got.InstanceID != w {
			t.Errorf("iter %d: got %q, want %q", i, got.InstanceID, w)
		}
	}
}

// TestPrefixAffinity_RepinsWhenPinnedReplicaGone: when a session's pinned
// replica drops out of the eligible set (quarantined / scaled away), the
// policy re-pins to a live replica instead of returning the dead one.
func TestPrefixAffinity_RepinsWhenPinnedReplicaGone(t *testing.T) {
	pa := policy.NewPrefixAffinity(0)
	reps := affinityReplicas()
	ctx := policy.WithSession(context.Background(), "sess-1")

	first, _ := pa.Pick(ctx, "d", reps, nil)

	reduced := make([]policy.Replica, 0, len(reps))
	for _, r := range reps {
		if r.InstanceID != first.InstanceID {
			reduced = append(reduced, r)
		}
	}
	got, ok := pa.Pick(ctx, "d", reduced, nil)
	if !ok {
		t.Fatal("re-pin Pick: ok=false")
	}
	if got.InstanceID == first.InstanceID {
		t.Errorf("returned the gone replica %q; expected a re-pin", first.InstanceID)
	}
}

// TestPrefixAffinity_EmptyEligibleReturnsNotOK: zero eligible replicas ->
// ok=false (router maps this to 503 replica_unavailable).
func TestPrefixAffinity_EmptyEligibleReturnsNotOK(t *testing.T) {
	pa := policy.NewPrefixAffinity(0)
	ctx := policy.WithSession(context.Background(), "sess-1")
	if _, ok := pa.Pick(ctx, "d", nil, nil); ok {
		t.Fatal("Pick on empty replicas should return ok=false")
	}
}

// fakeStats reports a fixed in-flight count per replica id.
type fakeStats map[string]int64

func (f fakeStats) InFlight(_, replicaID string) int64 { return f[replicaID] }

// TestPrefixAffinity_OverrideSpillsThenSnapsBack: with the override
// enabled, a hot pinned replica spills the turn to the coolest replica
// WITHOUT dropping the pin, so once the replica cools the session snaps
// back to it. The load-bearing "affinity tempered by load" contract.
func TestPrefixAffinity_OverrideSpillsThenSnapsBack(t *testing.T) {
	pa := policy.NewPrefixAffinity(4)
	reps := affinityReplicas() // a, b, c
	ctx := policy.WithSession(context.Background(), "sess-1")

	pinned, _ := pa.Pick(ctx, "d", reps, fakeStats{}) // pins (round-robin first) -> a

	// a is hot (>= 4), b is coolest -> spill to b, pin kept.
	if got, _ := pa.Pick(ctx, "d", reps, fakeStats{"a": 5, "b": 0, "c": 1}); got.InstanceID != "b" {
		t.Errorf("hot pin: got %q, want spill to coolest b", got.InstanceID)
	}
	// a cools -> snap back to the original pin.
	if got, _ := pa.Pick(ctx, "d", reps, fakeStats{"a": 0}); got.InstanceID != pinned.InstanceID {
		t.Errorf("after cooldown: got %q, want snap-back to pin %q", got.InstanceID, pinned.InstanceID)
	}
}

// TestPrefixAffinity_NoSpillBelowThreshold: in-flight under the threshold
// leaves the session on its pin.
func TestPrefixAffinity_NoSpillBelowThreshold(t *testing.T) {
	pa := policy.NewPrefixAffinity(4)
	reps := affinityReplicas()
	ctx := policy.WithSession(context.Background(), "sess-1")
	pinned, _ := pa.Pick(ctx, "d", reps, fakeStats{})
	if got, _ := pa.Pick(ctx, "d", reps, fakeStats{"a": 3, "b": 0}); got.InstanceID != pinned.InstanceID {
		t.Errorf("below threshold: got %q, want pin %q", got.InstanceID, pinned.InstanceID)
	}
}

// TestPrefixAffinity_OverrideDisabled: threshold 0 never spills, even when
// the pinned replica is slammed.
func TestPrefixAffinity_OverrideDisabled(t *testing.T) {
	pa := policy.NewPrefixAffinity(0)
	reps := affinityReplicas()
	ctx := policy.WithSession(context.Background(), "sess-1")
	pinned, _ := pa.Pick(ctx, "d", reps, fakeStats{})
	if got, _ := pa.Pick(ctx, "d", reps, fakeStats{"a": 999, "b": 0}); got.InstanceID != pinned.InstanceID {
		t.Errorf("override disabled: got %q, want pin %q despite load", got.InstanceID, pinned.InstanceID)
	}
}

// TestPrefixAffinity_NoSpillWhenPinnedIsCoolest: over threshold but the
// pin is itself the least-loaded -> nowhere cooler to go, keep the pin.
func TestPrefixAffinity_NoSpillWhenPinnedIsCoolest(t *testing.T) {
	pa := policy.NewPrefixAffinity(4)
	reps := affinityReplicas()
	ctx := policy.WithSession(context.Background(), "sess-1")
	pinned, _ := pa.Pick(ctx, "d", reps, fakeStats{})
	if got, _ := pa.Pick(ctx, "d", reps, fakeStats{"a": 5, "b": 6, "c": 7}); got.InstanceID != pinned.InstanceID {
		t.Errorf("pin is coolest: got %q, want pin %q", got.InstanceID, pinned.InstanceID)
	}
}

// TestPrefixAffinity_OverrideNilStatsSafe: the override is a no-op when no
// Stats is supplied (guard), returning the pin without panicking.
func TestPrefixAffinity_OverrideNilStatsSafe(t *testing.T) {
	pa := policy.NewPrefixAffinity(4)
	reps := affinityReplicas()
	ctx := policy.WithSession(context.Background(), "sess-1")
	pinned, _ := pa.Pick(ctx, "d", reps, nil)
	if got, _ := pa.Pick(ctx, "d", reps, nil); got.InstanceID != pinned.InstanceID {
		t.Errorf("nil stats: got %q, want pin %q", got.InstanceID, pinned.InstanceID)
	}
}
