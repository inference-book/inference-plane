package skucatalog

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

// A catalog with one row of each shape the three real adapters produce: a
// bridge-capable card (fabric UNKNOWN without a reading), a card with a
// board-integrated fabric, and a card with none at all.
func testCatalog() []Entry {
	return []Entry{
		{Token: "no-fabric-cheap", VRAMGb: 24, SystemRAMGb: 16, PriceUSDPerHour: 0.30, Family: fabric.FamilyRTX4090},
		{Token: "bridge-capable", VRAMGb: 80, SystemRAMGb: 128, PriceUSDPerHour: 1.20, Family: fabric.FamilyA100PCIe},
		{Token: "always-fabric", VRAMGb: 80, SystemRAMGb: 128, PriceUSDPerHour: 1.30, Family: fabric.FamilyA100SXM},
	}
}

// The rule that #281 cost us. A provider that publishes no figure for a sizing
// fact must not have its catalog narrowed by a constraint on that fact, because
// the row never bounded it. Lambda is the live case: it names no system RAM, so
// min_ram_gb cannot be judged against its rows.
func TestUnpublishedSizingFactDoesNotFilter(t *testing.T) {
	catalog := []Entry{
		{Token: "publishes-nothing", VRAMGb: 80, SystemRAMGb: 0, PriceUSDPerHour: 1.00, Family: fabric.FamilyA100SXM},
	}

	got := Match(catalog, &provisionerv1.ResourceRequirements{MinRamGb: 4096}, FabricDeclared)

	if len(got) != 1 {
		t.Fatalf("Match dropped a row over a fact its provider never published: got %v, want [publishes-nothing]", got)
	}
}

// The other half of the same rule: where a provider does publish the figure, it
// still filters exactly as before. Losing this would silently widen every
// RunPod and Vast resolution.
func TestPublishedSizingFactStillFilters(t *testing.T) {
	catalog := []Entry{
		{Token: "small-ram", VRAMGb: 80, SystemRAMGb: 64, PriceUSDPerHour: 1.00, Family: fabric.FamilyA100SXM},
		{Token: "big-ram", VRAMGb: 80, SystemRAMGb: 256, PriceUSDPerHour: 2.00, Family: fabric.FamilyA100SXM},
	}

	got := Match(catalog, &provisionerv1.ResourceRequirements{MinRamGb: 128}, FabricDeclared)

	if len(got) != 1 || got[0] != "big-ram" {
		t.Errorf("min_ram_gb=128 got %v, want [big-ram]", got)
	}
}

// GPUCount is the fact only Lambda publishes, because only Lambda's rows
// describe a whole instance. On per-card catalogs the count is a create param,
// so a row carrying 0 must stay eligible.
func TestGPUCountFiltersOnlyWhereItIsPublished(t *testing.T) {
	perCard := []Entry{{Token: "per-card", VRAMGb: 80, PriceUSDPerHour: 1.00, Family: fabric.FamilyA100SXM}}
	perInstance := []Entry{
		{Token: "one-card", VRAMGb: 80, GPUCount: 1, PriceUSDPerHour: 1.00, Family: fabric.FamilyA100SXM},
		{Token: "eight-card", VRAMGb: 80, GPUCount: 8, PriceUSDPerHour: 8.00, Family: fabric.FamilyA100SXM},
	}
	reqs := &provisionerv1.ResourceRequirements{GpuCount: 4}

	if got := Match(perCard, reqs, FabricDeclared); len(got) != 1 {
		t.Errorf("per-card catalog got %v, want the row to survive a gpu_count it does not describe", got)
	}
	if got := Match(perInstance, reqs, FabricDeclared); len(got) != 1 || got[0] != "eight-card" {
		t.Errorf("per-instance catalog got %v, want [eight-card]", got)
	}
}

// A capability we cannot vouch for fails closed, which is the opposite of the
// sizing rule and the whole reason both are stated. On a provider with no
// readings a bridge-capable card is UNKNOWN, and renting one to find out costs
// the bill whether or not the link was there.
func TestDeclaredModeDropsUnvouchedFabric(t *testing.T) {
	reqs := &provisionerv1.ResourceRequirements{
		FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	}

	got := Match(testCatalog(), reqs, FabricDeclared)

	if len(got) != 1 || got[0] != "always-fabric" {
		t.Errorf("FabricDeclared got %v, want [always-fabric] only", got)
	}
}

// On a provider that measures every candidate, the same catalog must stay
// searchable, because the reading is what decides. Three of 24 "A100 PCIE"
// offers in the 2026-08-09 Vast probe reported a real link, so dropping the
// bridge-capable row here discards exactly the hosts worth having.
func TestPrefilterModeKeepsBridgeCapableFabric(t *testing.T) {
	reqs := &provisionerv1.ResourceRequirements{
		FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	}

	got := Match(testCatalog(), reqs, FabricPrefilter)

	if len(got) != 2 {
		t.Fatalf("FabricPrefilter got %v, want both fabric-capable rows", got)
	}
	if got[0] != "bridge-capable" || got[1] != "always-fabric" {
		t.Errorf("FabricPrefilter got %v, want [bridge-capable always-fabric]", got)
	}
}

// A bandwidth floor is a claim about a specific machine, so it can only be
// applied where the tier has been vouched for. In prefilter mode the catalog
// figure is not yet evidence about any host, and findOffer pushes the real
// bandwidth filter server-side.
func TestPrefilterModeLeavesBandwidthToTheProvider(t *testing.T) {
	reqs := &provisionerv1.ResourceRequirements{
		FabricScope:   provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
		MinFabricGbps: 99999,
	}

	if got := Match(testCatalog(), reqs, FabricPrefilter); len(got) != 2 {
		t.Errorf("prefilter got %v, want the bandwidth floor deferred to the offer search", got)
	}
	if got := Match(testCatalog(), reqs, FabricDeclared); len(got) != 0 {
		t.Errorf("declared got %v, want an unreachable bandwidth floor to match nothing", got)
	}
}

// Cheapest first, and ties hold catalog order so a curated ordering is not
// scrambled by a sort that had no reason to move anything.
func TestOrdersByPriceAndHoldsTies(t *testing.T) {
	catalog := []Entry{
		{Token: "expensive", VRAMGb: 80, PriceUSDPerHour: 3.00, Family: fabric.FamilyA100SXM},
		{Token: "tie-first", VRAMGb: 80, PriceUSDPerHour: 1.00, Family: fabric.FamilyA100SXM},
		{Token: "tie-second", VRAMGb: 80, PriceUSDPerHour: 1.00, Family: fabric.FamilyA100SXM},
	}

	got := Match(catalog, &provisionerv1.ResourceRequirements{}, FabricDeclared)

	want := []string{"tie-first", "tie-second", "expensive"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// The cap is what stops an unbounded class shorthand walking up into frontier
// hardware once the cheap tiers are busy, so it must survive on the cheap end.
//
// The catalog is built most-expensive-first on purpose. Capping before the
// sort rather than after would then keep the priciest rows and still return
// MaxResults of them, which a length check alone would wave through.
func TestCapsAtMaxResultsKeepingTheCheapest(t *testing.T) {
	total := MaxResults + 3
	var catalog []Entry
	for i := range total {
		catalog = append(catalog, Entry{
			Token:           string(rune('a' + i)),
			VRAMGb:          80,
			PriceUSDPerHour: float64(total - i),
			Family:          fabric.FamilyA100SXM,
		})
	}

	got := Match(catalog, &provisionerv1.ResourceRequirements{}, FabricDeclared)

	if len(got) != MaxResults {
		t.Fatalf("got %d results, want %d", len(got), MaxResults)
	}
	cheapest := string(rune('a' + total - 1))
	if got[0] != cheapest {
		t.Errorf("cheapest survivor is %q, want %q; the cap must be applied after the sort, not before", got[0], cheapest)
	}
}

// Nil requirements is the "no spec at all" call, distinct from an empty spec
// that matches everything. Adapters lean on the distinction.
func TestNilRequirementsMatchNothing(t *testing.T) {
	if got := Match(testCatalog(), nil, FabricDeclared); got != nil {
		t.Errorf("got %v, want nil", got)
	}
	if got := Match(testCatalog(), &provisionerv1.ResourceRequirements{}, FabricDeclared); len(got) != 3 {
		t.Errorf("empty requirements got %v, want the whole catalog", got)
	}
}

// An impossible constraint yields an empty result rather than a nil-vs-empty
// surprise, because every adapter branches on len()==0 to raise "no matching
// SKU" instead of handing nothing to the provider.
func TestNoMatchReturnsEmpty(t *testing.T) {
	got := Match(testCatalog(), &provisionerv1.ResourceRequirements{MinVramGb: 999}, FabricDeclared)

	if len(got) != 0 {
		t.Errorf("got %v, want no matches", got)
	}
}
