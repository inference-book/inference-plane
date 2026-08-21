package lambdalabs

import (
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// This adapter consults a provider-side registry, so its Describe answering
// "not found" is evidence the instance is gone rather than evidence it was
// never tracked. Declaring that is what lets a stale record reconcile
// instead of sitting ACTIVE forever (#396).
//
// A compile-time assertion, because an adapter that silently stops
// satisfying the interface would fail closed: the record would simply never
// reconcile, and nothing else would say so.
var _ provisioners.InstanceTracker = (*Provider)(nil)

func TestProviderTracksInstances(t *testing.T) {
	var p *Provider
	if !p.TracksInstances() {
		t.Error("TracksInstances() = false, want true: this provider has a registry to ask")
	}
}
