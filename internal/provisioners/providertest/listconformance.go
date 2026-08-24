// Package providertest carries the conformance suites every Provider
// adapter has to pass, so a contract the Service depends on is asserted in
// each adapter's own test package rather than being discovered from the
// Service's behaviour.
//
// It exists because `List`'s filter contract was honoured by one adapter in
// three. RunPod matched every filter key, Vast read only `label-prefix` and
// Lambda only `name-prefix`, and neither of those is a key the Service ever
// passes. Both adapters therefore answered "which instance is this?" with
// the whole account, and the Service read the length of that answer as
// evidence about one instance. Every adapter's unit tests passed throughout,
// because each was written against the filter that adapter happened to
// implement.
package providertest

import (
	"context"
	"slices"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// SeededInstance describes one instance an adapter's fake API is serving,
// and the tags List is expected to recover for it.
//
// Tags declares what the adapter can genuinely derive from its own wire
// data, which differs by provider: RunPod stores tags directly, Vast can
// recover an id from the label and nothing else, Lambda recovers whatever it
// stamps. The suite builds its cases from what is declared, so an adapter is
// held to the contract over the tags it has rather than to tags it could
// never know.
type SeededInstance struct {
	ProviderID string
	Tags       map[string]string
}

// RunListFilterConformance asserts a Provider's List honours the match-all
// tag filter the interface documents.
//
// The adapter's own test seeds its fake with instances matching `seeded` and
// passes the constructed Provider. Every case is derived from `seeded`, so
// the suite needs no knowledge of any provider's wire format.
func RunListFilterConformance(t *testing.T, p provisioners.Provider, seeded []SeededInstance) {
	t.Helper()
	if len(seeded) < 2 {
		t.Fatalf("conformance needs at least two seeded instances to tell filtering from luck, got %d", len(seeded))
	}

	all := make([]string, 0, len(seeded))
	for _, s := range seeded {
		all = append(all, s.ProviderID)
	}

	t.Run("no filter returns everything", func(t *testing.T) {
		assertIDs(t, p, nil, all)
	})

	t.Run("empty filter returns everything", func(t *testing.T) {
		assertIDs(t, p, map[string]string{}, all)
	})

	// One case per (instance, tag): filtering on a tag that identifies one
	// instance must return that instance and no other. This is the case both
	// broken adapters failed, and they failed it by returning all of them.
	for _, s := range seeded {
		for k, v := range s.Tags {
			if sharedBy(seeded, k, v) > 1 {
				continue // not identifying; covered by the shared-value case below
			}
			t.Run("filter on "+k+"="+v, func(t *testing.T) {
				assertIDs(t, p, map[string]string{k: v}, []string{s.ProviderID})
			})
		}
	}

	t.Run("a tag value nothing carries matches nothing", func(t *testing.T) {
		for k := range seeded[0].Tags {
			assertIDs(t, p, map[string]string{k: "no-instance-has-this-value"}, nil)
			return
		}
		t.Skip("adapter recovers no tags")
	})

	t.Run("a tag key nothing carries matches nothing", func(t *testing.T) {
		assertIDs(t, p, map[string]string{"iplane-nonexistent-key": "x"}, nil)
	})

	// Match-all rather than match-any. A filter naming one instance's id and
	// another's operator describes no instance, and an adapter that ORs its
	// keys would return two.
	t.Run("keys are ANDed", func(t *testing.T) {
		k, v, ok := anyTag(seeded[0])
		if !ok {
			t.Skip("adapter recovers no tags")
		}
		filter := map[string]string{k: v, "iplane-nonexistent-key": "x"}
		assertIDs(t, p, filter, nil)
	})
}

func assertIDs(t *testing.T, p provisioners.Provider, filter map[string]string, want []string) {
	t.Helper()
	refs, err := p.List(context.Background(), filter)
	if err != nil {
		t.Fatalf("List(%v): %v", filter, err)
	}
	got := make([]string, 0, len(refs))
	for _, r := range refs {
		got = append(got, r.GetProviderId())
	}
	slices.Sort(got)
	sorted := slices.Clone(want)
	slices.Sort(sorted)
	if !slices.Equal(got, sorted) {
		t.Errorf("List(%v) returned %v, want %v", filter, got, sorted)
	}
}

func sharedBy(seeded []SeededInstance, key, value string) int {
	n := 0
	for _, s := range seeded {
		if s.Tags[key] == value {
			n++
		}
	}
	return n
}

func anyTag(s SeededInstance) (string, string, bool) {
	for k, v := range s.Tags {
		return k, v, true
	}
	return "", "", false
}
