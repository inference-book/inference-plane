package fabric

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Stamp writes all four fields or none. A partial stamp is the failure it
// exists to prevent: a scope with no source is a claim with no provenance, and
// Satisfies would then read it as vouched-for.
func TestStampWritesTheWholeVerdict(t *testing.T) {
	hw := &provisionerv1.Hardware{}

	Stamp(hw, Observation{Family: FamilyA100SXM})

	if hw.FabricScope != provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE {
		t.Errorf("scope = %v, want INTRA_NODE", hw.FabricScope)
	}
	if hw.FabricSource != provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED {
		t.Errorf("source = %v, want DECLARED", hw.FabricSource)
	}
	if hw.FabricGbps == 0 {
		t.Error("gbps = 0, want the catalog's peak for a board-integrated fabric")
	}
	if hw.FabricTechnology == "" {
		t.Error("technology is empty, want the catalog's name for the fabric")
	}
}

// An uncatalogued card must leave UNKNOWN behind rather than a stale or
// zero-valued verdict that reads as DECLARED-none.
func TestStampLeavesUnknownForAnUncataloguedCard(t *testing.T) {
	hw := &provisionerv1.Hardware{
		FabricScope:      provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
		FabricSource:     provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED,
		FabricGbps:       4800,
		FabricTechnology: "nvlink",
	}

	Stamp(hw, Observation{Family: Family("no-such-card")})

	if hw.FabricSource != provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN {
		t.Errorf("source = %v, want UNKNOWN to overwrite the prior verdict", hw.FabricSource)
	}
	if hw.FabricGbps != 0 || hw.FabricTechnology != "" {
		t.Errorf("gbps=%d technology=%q, want both cleared alongside the scope",
			hw.FabricGbps, hw.FabricTechnology)
	}
}

// Adapters call Stamp on paths where the Hardware record may not have been
// built yet, so a nil target is a no-op rather than a panic on a provisioning
// path that was otherwise fine.
func TestStampToleratesNilHardware(t *testing.T) {
	Stamp(nil, Observation{Family: FamilyA100SXM})
}
