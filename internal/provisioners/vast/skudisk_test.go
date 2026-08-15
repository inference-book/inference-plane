package vast

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// DefaultDiskGb is a typical default for the tier, not a ceiling the card
// imposes. Disk is an independent create param: Spawn reads min_disk_gb
// straight off the requirements and hands it to findOffer, which pushes it
// into the offer search. So a catalog-stage disk filter rejects hardware that
// would have served, and a 72B FP8 asking for 150 GB (what
// examples/08-scaling-30b sets) finds nothing at all on a marketplace full of
// capacity. RunPod removed the same filter during the 72B cold-start work.
// See issue 281.
func TestMinDiskDoesNotFilterTheCatalog(t *testing.T) {
	unconstrained := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 80})
	withDisk := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 80, MinDiskGb: 150})

	if len(withDisk) == 0 {
		t.Fatalf("min_disk_gb=150 matched no SKU; unconstrained matched %v", unconstrained)
	}
	if len(withDisk) != len(unconstrained) {
		t.Errorf("min_disk_gb=150 changed the match set: got %v, want %v", withDisk, unconstrained)
	}
}

// The quieter half of the same bug. At min_disk_gb=100 the filter dropped
// every 80 GB SKU and left the resolver starting at H100 NVL, so an operator
// who asked for a large card and a big disk silently got a more expensive
// tier rather than an error.
func TestMinDiskDoesNotEscalatePriceTier(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 80, MinDiskGb: 100})

	if len(got) == 0 {
		t.Fatal("min_disk_gb=100 matched no SKU")
	}
	cheapest := LookupSKU(got[0])
	if cheapest == nil {
		t.Fatalf("matched gpu %q not in catalog", got[0])
	}
	if cheapest.VRAMGb != 80 {
		t.Errorf("cheapest match is %s at %d GB VRAM, want an 80 GB part; the disk filter escalated the tier",
			got[0], cheapest.VRAMGb)
	}
}
