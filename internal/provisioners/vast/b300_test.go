package vast

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/skucatalog"
)

// Eight B300s hold 2304 GB, which is the single-node route to a model that
// otherwise needs a cross-node pool: Kimi K3 is 1560 GB at four bits and
// does not fit eight of anything smaller. Vast had a rentable 8x B300 at
// $90.01/hr on 2026-08-21 and the catalog could not name the card, so
// `iplane capacity --gpu-count 8 --min-vram-gb 200` answered nothing (#354).
func TestMatchSKUsOffersB300ForAFrontierRequest(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 200, GpuCount: 8})

	found := false
	for _, name := range got {
		if name == "B300" {
			found = true
		}
		if spec := LookupSKU(name); spec != nil && spec.VRAMGb < 200 {
			t.Errorf("SKU %q has VRAM=%d, below the 200 GB floor", name, spec.VRAMGb)
		}
	}
	if !found {
		t.Errorf("MatchSKUs = %v, want B300 among them", got)
	}
}

// The label is decimal, unlike an A100's "80GB" which is 80 GiB. Vast
// reports the card as 275040 MiB, which is 268.6 GiB and 288.4 decimal GB,
// so reading "288" as a binary count would promise 309 GB per card and
// over-commit every B300 budget by twenty gigabytes.
//
// This pins a deliberate absence from skucatalog's binaryLabels, in the
// same family as H200's 141 and Blackwell's 180 and 192.
func TestB300HasNoExactCapacityBecauseItsLabelIsDecimal(t *testing.T) {
	spec := LookupSKU("B300")
	if spec == nil {
		t.Fatal("B300 is not in the catalog")
	}
	if got := skucatalog.ExactVRAMBytes(spec.VRAMGb); got != 0 {
		t.Errorf("ExactVRAMBytes(%d) = %d, want 0: the label is a vendor decimal figure, not a binary count",
			spec.VRAMGb, got)
	}
}

// The floor a SKU pushes into the offer search has to sit under what the
// card actually reports, for every card in the catalog. It did not: one
// constant was converting a unit and allowing a tolerance at the same time,
// and the conversion only held for labels that read as binary. Every
// Blackwell card was excluded from every search, silently, since the row
// was written (#401).
//
// The figures are measured, from live offers on 2026-08-21 and from the
// earlier A100 and H200 readings the catalog was tuned against.
func TestVRAMFloorSitsUnderWhatEveryCardReports(t *testing.T) {
	for _, c := range []struct {
		sku         string
		reportedMiB int
	}{
		{"A100_SXM4", 81920},
		{"H200", 143771},
		{"B200", 183359},
		{"B300", 275040},
	} {
		floor := offerVRAMFloorMB(c.sku, &provisionerv1.ResourceRequirements{})
		if floor > c.reportedMiB {
			spec := LookupSKU(c.sku)
			t.Errorf("%s: floor %d MiB is above the %d MiB the card reports, so the search excludes it (label %d)",
				c.sku, floor, c.reportedMiB, spec.VRAMGb)
		}
	}
}

// And it still has to keep the smaller part out, which is what the floor
// was added for: asking for an 80 GB A100 must never return the 40 GB one
// sharing its wire name (#243).
func TestVRAMFloorStillExcludesTheSmallerVariant(t *testing.T) {
	floor := offerVRAMFloorMB("A100_SXM4", &provisionerv1.ResourceRequirements{})
	const fortyGBCardReports = 40960
	if floor <= fortyGBCardReports {
		t.Errorf("floor %d MiB admits a 40 GB card reporting %d", floor, fortyGBCardReports)
	}
}
