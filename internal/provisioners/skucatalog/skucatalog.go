// Package skucatalog holds the resolver every provider adapter runs over its
// own GPU catalog: filter the rows by the operator's numeric constraints, drop
// what cannot carry the requested fabric, order by price, cap the list.
//
// It lives outside the adapters for the reason the fabric package does. The
// catalog DATA is legitimately per-provider, since gpu_name tokens, prices and
// which cards a vendor actually sells are not shared facts. The ALGORITHM over
// that data is not per-provider, and writing it once per adapter is how the
// three copies drifted: RunPod learned during the 72B cold-start work that
// min_disk_gb must not filter the catalog, and Vast kept filtering on it for
// another two chapters (issues 266, 281).
//
// Ch 11 is the other half of the reason. Resolving one requirement against
// several catalogs at once is this same algorithm over N sets of rows, which
// is what a shared resolver gives us and three adapter-local copies do not.
package skucatalog

import (
	"sort"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

// Entry is one catalog row reduced to the facts the resolver reasons about.
// Adapters project their own SKU type onto this and keep everything else,
// including the provider's wire naming and any offer-level filtering, to
// themselves.
//
// A zero on SystemRAMGb or GPUCount means the provider publishes no figure,
// which is different from publishing a zero and is why those two carry the
// rule spelled out on Match. VRAMGb has no such state: every catalog we have
// names the memory on the card, and a row that did not could not be matched
// against a model size at all.
type Entry struct {
	// Token is what the resolver emits: the provider's own SKU identifier,
	// in whatever form that provider's create call expects.
	Token string

	// VRAMGb is GPU memory per card. Always published, always filters.
	VRAMGb int

	// SystemRAMGb is the system RAM a typical single-GPU host of this tier
	// carries. 0 when the provider publishes none (Lambda).
	SystemRAMGb int

	// GPUCount is the number of cards on the rented shape, for catalogs whose
	// rows describe a whole instance rather than a card. 0 when the rows are
	// per-card and the count is a create param instead (RunPod, Vast).
	GPUCount int

	// PriceUSDPerHour orders the matching set. Indicative at catalog time; the
	// authoritative price comes back from the provider on the create call.
	PriceUSDPerHour float64

	// Family maps the row onto the cross-provider fabric catalog.
	Family fabric.Family
}

// FabricMode selects how much the catalog stage is allowed to conclude about a
// fabric requirement, which is the one filter that genuinely differs by
// provider rather than by accident.
type FabricMode int

const (
	// FabricDeclared is for providers that report no interconnect measurement,
	// so the catalog is the last word. A bridge-capable card resolves to
	// UNKNOWN and is dropped, because renting one to find out costs money.
	// RunPod and Lambda.
	FabricDeclared FabricMode = iota

	// FabricPrefilter is for providers that measure the fabric per candidate.
	// The catalog drops only what could never carry the requested fabric and
	// leaves the uncertain rows searchable, because the per-offer reading is
	// what actually decides. Dropping bridge-capable cards here would discard
	// exactly the machines worth having. Vast.
	FabricPrefilter
)

// MaxResults caps how many SKUs a resolution returns.
//
// Class shorthand has no upper bound today (class=small expands to
// min_vram_gb=24 with no maximum), so without a cap every card above the floor
// enters the candidate list, B200 included. An operator who asked for "small"
// should not land on a frontier GPU because the cheap tiers were busy. Capping
// at the cheapest few preserves real fallback without exposing the caller to a
// large price-tier jump. Adapters re-export this under their own name with a
// note on how each of them consumes the list.
const MaxResults = 5

// Match returns the tokens whose rows satisfy every stated constraint,
// cheapest first, capped at MaxResults. It returns an empty slice when nothing
// matches, which adapters surface as "no matching SKU" rather than passing an
// empty list on to the provider.
//
// Two rules decide what a missing fact means, and they point in opposite
// directions on purpose.
//
// A SIZING fact the provider does not publish cannot filter. Lambda names no
// system RAM, so min_ram_gb does not narrow its catalog. The alternative is
// rejecting hardware over a number nobody stated, which is how the disk filter
// (#281) blocked a 72B deploy on Vast against a marketplace full of capacity.
// Note this makes such a constraint silently unenforced on that provider, a
// real cost tracked separately as #283.
//
// A CAPABILITY we cannot vouch for fails closed. That is the fabric doctrine
// already established in the fabric package: an unknown source never satisfies
// a stated requirement, because the failure mode runs the other way. Renting a
// pool and discovering afterwards that the interconnect was never there costs
// the whole bill.
//
// The distinction is what a catalog row can bound. VRAM and fabric are
// properties of the card, so the row is the right place to judge them. Disk
// and RAM are properties of the rented shape, ordered alongside the card, so a
// row's figure for them is a typical default and never a ceiling.
func Match(entries []Entry, reqs *provisionerv1.ResourceRequirements, mode FabricMode) []string {
	if reqs == nil {
		return nil
	}

	var matches []Entry
	for _, e := range entries {
		if e.VRAMGb < int(reqs.GetMinVramGb()) {
			continue
		}
		if e.SystemRAMGb > 0 && int(reqs.GetMinRamGb()) > 0 && e.SystemRAMGb < int(reqs.GetMinRamGb()) {
			continue
		}
		if e.GPUCount > 0 && reqs.GetGpuCount() > 0 && e.GPUCount < int(reqs.GetGpuCount()) {
			continue
		}
		if !satisfiesFabric(e.Family, reqs, mode) {
			continue
		}
		matches = append(matches, e)
	}

	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].PriceUSDPerHour < matches[j].PriceUSDPerHour
	})
	if len(matches) > MaxResults {
		matches = matches[:MaxResults]
	}

	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.Token
	}
	return out
}

// satisfiesFabric applies whichever of the fabric package's two verdicts this
// provider has earned the right to. CouldSatisfy reads scope only, since a
// bandwidth floor on a row we have not measured would be comparing a
// requirement against a catalog figure the host may not deliver.
func satisfiesFabric(family fabric.Family, reqs *provisionerv1.ResourceRequirements, mode FabricMode) bool {
	if mode == FabricPrefilter {
		return fabric.CouldSatisfy(family, reqs.GetFabricScope())
	}
	return fabric.Satisfies(
		fabric.Resolve(fabric.Observation{Family: family}),
		reqs.GetFabricScope(), reqs.GetMinFabricGbps(),
	)
}
