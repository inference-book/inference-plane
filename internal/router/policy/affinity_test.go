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
	pa := policy.NewPrefixAffinity()
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
	pa := policy.NewPrefixAffinity()
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
	pa := policy.NewPrefixAffinity()
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
	pa := policy.NewPrefixAffinity()
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
	pa := policy.NewPrefixAffinity()
	ctx := policy.WithSession(context.Background(), "sess-1")
	if _, ok := pa.Pick(ctx, "d", nil, nil); ok {
		t.Fatal("Pick on empty replicas should return ok=false")
	}
}
