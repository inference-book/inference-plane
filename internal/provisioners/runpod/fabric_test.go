package runpod

import (
	"slices"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

// Every catalog SKU must carry a Family the fabric package recognizes.
// Without this, a new SKU row silently resolves to UNKNOWN and quietly
// disappears from every fabric-constrained request.
func TestEverySKUHasAKnownFamily(t *testing.T) {
	for _, sku := range skus {
		if sku.Family == "" {
			t.Errorf("%q has no fabric.Family", sku.GpuTypeID)
			continue
		}
		res := fabric.Resolve(fabric.Observation{Family: sku.Family})
		if res.Source == provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN &&
			res.Scope == provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED {
			// UNKNOWN is legitimate for bridge-capable parts, but not for a
			// family the catalog has never heard of. Distinguish by asking
			// whether the family is present at all.
			if _, known := fabricCatalogHas(sku.Family); !known {
				t.Errorf("%q maps to unknown family %q", sku.GpuTypeID, sku.Family)
			}
		}
	}
}

func fabricCatalogHas(f fabric.Family) (struct{}, bool) {
	// Resolve reports UNKNOWN both for "not in catalog" and for
	// "bridge-capable, unmeasured". A measurement forces a verdict only in
	// the second case, which separates them without exporting the catalog.
	res := fabric.Resolve(fabric.Observation{Family: f, HasMeasurement: true, MeasuredGbps: 1})
	return struct{}{}, res.Source == provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED
}

func TestMatchSKUsIntraNodeFilter(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{
		MinVramGb:   80,
		GpuCount:    2,
		FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	})

	// SXM and NVL parts are the only >=80 GB SKUs whose fabric RunPod's
	// naming actually settles.
	for _, want := range []string{"NVIDIA A100-SXM4-80GB", "NVIDIA H100 80GB HBM3"} {
		if !slices.Contains(got, want) {
			t.Errorf("intra-node match missing %q; got %v", want, got)
		}
	}

	// The row worth pressure-testing. These cards CAN be bridged, RunPod does
	// not say whether they are, so they must not satisfy the requirement.
	// Silently including them is the bill-before-answer failure the filter
	// exists to prevent.
	for _, unwanted := range []string{"NVIDIA A100 80GB PCIe", "NVIDIA H100 PCIe"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("intra-node match wrongly included bridge-capable %q; got %v", unwanted, got)
		}
	}
}

func TestMatchSKUsRejectsCardsWithNoFabric(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{
		MinVramGb:   24,
		GpuCount:    2,
		FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	})
	for _, sku := range got {
		switch sku {
		case "NVIDIA GeForce RTX 4090", "NVIDIA GeForce RTX 5090", "NVIDIA L4",
			"NVIDIA L40", "NVIDIA L40S", "NVIDIA A40", "NVIDIA RTX 6000 Ada Generation":
			t.Errorf("%q has no NVLink at all but satisfied an intra-node request", sku)
		}
	}
}

// The Ch 6-9 default must stay free: no fabric requirement means every SKU
// that meets the numeric constraints is still a candidate.
func TestMatchSKUsUnconstrainedIsUnchanged(t *testing.T) {
	reqs := &provisionerv1.ResourceRequirements{MinVramGb: 80}
	got := MatchSKUs(reqs)
	if len(got) == 0 {
		t.Fatal("unconstrained 80GB request returned no SKUs")
	}
	if !slices.Contains(got, "NVIDIA A100 80GB PCIe") {
		t.Errorf("unconstrained request dropped a PCIe SKU; fabric filter should be inert here. got %v", got)
	}
}

func TestMatchSKUsBandwidthFloor(t *testing.T) {
	// 7200 Gbps (900 GB/s) admits the NVSwitch parts and excludes A100 SXM
	// at 4800. Coarse on purpose: the catalog comment explains why fine
	// thresholds near a card's rating are not dependable.
	got := MatchSKUs(&provisionerv1.ResourceRequirements{
		MinVramGb:     80,
		GpuCount:      2,
		FabricScope:   provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
		MinFabricGbps: 7200,
	})
	if slices.Contains(got, "NVIDIA A100-SXM4-80GB") {
		t.Errorf("A100 SXM (4800 Gbps) passed a 7200 Gbps floor; got %v", got)
	}
	if !slices.Contains(got, "NVIDIA H100 80GB HBM3") {
		t.Errorf("H100 SXM (7200 Gbps) failed a 7200 Gbps floor; got %v", got)
	}
}

func TestStampFabric(t *testing.T) {
	tests := []struct {
		sku        string
		wantScope  provisionerv1.FabricScope
		wantSource provisionerv1.FabricSource
		wantTech   string
	}{
		{"NVIDIA A100-SXM4-80GB",
			provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
			provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED, "nvlink"},
		{"NVIDIA L40S",
			provisionerv1.FabricScope_FABRIC_SCOPE_NONE,
			provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED, ""},
		{"NVIDIA A100 80GB PCIe",
			provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN, ""},
		// Operator-supplied SKU outside the catalog: no opinion, not a guess.
		{"NVIDIA SOME FUTURE GPU",
			provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN, ""},
	}
	for _, tt := range tests {
		t.Run(tt.sku, func(t *testing.T) {
			hw := &provisionerv1.Hardware{GpuSku: tt.sku}
			stampFabric(hw, tt.sku)
			if hw.GetFabricScope() != tt.wantScope {
				t.Errorf("scope = %v, want %v", hw.GetFabricScope(), tt.wantScope)
			}
			if hw.GetFabricSource() != tt.wantSource {
				t.Errorf("source = %v, want %v", hw.GetFabricSource(), tt.wantSource)
			}
			if hw.GetFabricTechnology() != tt.wantTech {
				t.Errorf("technology = %q, want %q", hw.GetFabricTechnology(), tt.wantTech)
			}
		})
	}
}

// The SXM/PCIe split this whole ticket rests on has to be readable from the
// gpuTypeId string, since RunPod exposes nothing else. If RunPod ever renames
// a SKU out of that convention, this is the test that says so.
func TestSKUNamingStillEncodesFormFactor(t *testing.T) {
	for _, sku := range skus {
		res := fabric.Resolve(fabric.Observation{Family: sku.Family})
		if res.Source != provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED ||
			res.Scope != provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE {
			continue
		}
		id := strings.ToUpper(sku.GpuTypeID)
		if !strings.Contains(id, "SXM") && !strings.Contains(id, "NVL") &&
			!strings.Contains(id, "HBM3") && !strings.Contains(id, "B200") &&
			!strings.Contains(id, "H200") {
			t.Errorf("%q resolves to a declared intra-node fabric but its id encodes no form factor", sku.GpuTypeID)
		}
	}
}
