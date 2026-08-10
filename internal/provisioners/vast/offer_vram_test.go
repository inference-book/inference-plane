package vast

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Vast lists physically different cards under one gpu_name. "A100 PCIE"
// covers both the 40 GB and the 80 GB part, and offers come back ordered by
// price, so the cheaper 40 GB one is what an unfiltered search returns
// first. Renting it would give half the VRAM the catalog claims for that
// SKU, and a model sized against the catalog would OOM on arrival.
func TestOfferVRAMFloorComesFromTheNamedSKU(t *testing.T) {
	got := offerVRAMFloorMB("A100_PCIE", &provisionerv1.ResourceRequirements{})

	if got == 0 {
		t.Fatal("no VRAM floor for a catalogued SKU; a 40GB offer would match an 80GB SKU")
	}
	// Must exclude the 40 GB part (40960 MB) and admit the 80 GB one
	// (81920 MB), including hosts that under-report slightly.
	if got <= 40960 {
		t.Errorf("floor = %d MB, want > 40960 so the 40GB part is excluded", got)
	}
	if got > 81920 {
		t.Errorf("floor = %d MB, want <= 81920 so genuine 80GB offers still match", got)
	}
}

// A host reporting 81251 MB is an 80 GB card. A floor pinned to the round
// number would reject it and the operator would see "no capacity" against a
// marketplace full of capacity.
func TestOfferVRAMFloorToleratesUnderReporting(t *testing.T) {
	got := offerVRAMFloorMB("A100_SXM4", &provisionerv1.ResourceRequirements{})

	if got > 81251 {
		t.Errorf("floor = %d MB, want <= 81251 so slightly under-reporting 80GB hosts match", got)
	}
}

// Asking for a SKU is asking for that card; asking for more memory than it
// has is a separate, stricter request. Both are real, so the larger wins.
func TestExplicitMinVramRaisesTheFloor(t *testing.T) {
	reqs := &provisionerv1.ResourceRequirements{MinVramGb: 140}

	got := offerVRAMFloorMB("A100_PCIE", reqs)

	if got <= 81920 {
		t.Errorf("floor = %d MB, want the operator's larger request to win", got)
	}
}

// An operator-supplied SKU that is not in the catalog still gets whatever
// floor they asked for, and no invented one when they asked for nothing.
func TestUncataloguedSKU(t *testing.T) {
	if got := offerVRAMFloorMB("SOME_FUTURE_CARD", &provisionerv1.ResourceRequirements{}); got != 0 {
		t.Errorf("floor = %d, want 0 for an unknown SKU with no explicit request", got)
	}
	reqs := &provisionerv1.ResourceRequirements{MinVramGb: 24}
	if got := offerVRAMFloorMB("SOME_FUTURE_CARD", reqs); got == 0 {
		t.Error("explicit min_vram_gb ignored for an unknown SKU")
	}
}
