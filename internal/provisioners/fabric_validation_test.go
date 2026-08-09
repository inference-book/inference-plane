package provisioners

import (
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

func fabricSpec(scope provisionerv1.FabricScope, gpus, gbps int32) *provisionerv1.Spec {
	return &provisionerv1.Spec{
		Requirements: &provisionerv1.ResourceRequirements{
			MinVramGb:     80,
			GpuCount:      gpus,
			FabricScope:   scope,
			MinFabricGbps: gbps,
		},
	}
}

// A fabric describes how cards reach each other, so asking for one on a
// single-GPU instance is a mistake. Rejecting beats silently ignoring: a
// quietly dropped constraint is the exact failure the filter exists to stop.
func TestValidateFabricNeedsTwoGPUs(t *testing.T) {
	intra := provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE
	for _, gpus := range []int32{0, 1} {
		err := ValidateAndExpandRequirements(fabricSpec(intra, gpus, 0))
		if err == nil {
			t.Errorf("gpu_count=%d with a fabric requirement was accepted", gpus)
			continue
		}
		if !strings.Contains(err.Error(), "gpu_count >= 2") {
			t.Errorf("gpu_count=%d error %q should name the constraint", gpus, err)
		}
	}
	if err := ValidateAndExpandRequirements(fabricSpec(intra, 2, 0)); err != nil {
		t.Errorf("gpu_count=2 with a fabric requirement was rejected: %v", err)
	}
}

// NONE means "PCIe is fine", which is a coherent thing to say about one card.
func TestValidateFabricNoneIsFineOnOneGPU(t *testing.T) {
	spec := fabricSpec(provisionerv1.FabricScope_FABRIC_SCOPE_NONE, 1, 0)
	if err := ValidateAndExpandRequirements(spec); err != nil {
		t.Errorf("fabric=none on a single GPU was rejected: %v", err)
	}
}

// Inter-node is a valid ask that nothing can supply yet. The error has to say
// that, not "no matching SKU", which reads as capacity an operator could wait out.
func TestValidateInterNodeSaysWhyNotJustNoMatch(t *testing.T) {
	spec := fabricSpec(provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE, 2, 0)
	err := ValidateAndExpandRequirements(spec)
	if err == nil {
		t.Fatal("inter-node request was accepted; no provider can rent a cross-node pool")
	}
	if !strings.Contains(err.Error(), "cross-node fabric") {
		t.Errorf("error %q should explain that no provider supplies this", err)
	}
}

func TestValidateBandwidthFloorNeedsAScope(t *testing.T) {
	spec := fabricSpec(provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED, 2, 4800)
	if err := ValidateAndExpandRequirements(spec); err == nil {
		t.Error("min_fabric_gbps without a fabric_scope was accepted")
	}
}

// The Ch 6-9 default must stay free.
func TestValidateNoFabricRequirementUnchanged(t *testing.T) {
	spec := fabricSpec(provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED, 1, 0)
	if err := ValidateAndExpandRequirements(spec); err != nil {
		t.Errorf("plain single-GPU request was rejected: %v", err)
	}
}
