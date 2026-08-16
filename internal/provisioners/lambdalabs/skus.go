package lambdalabs

import (
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
	"github.com/inference-book/inference-plane/internal/provisioners/skucatalog"
)

// SKUSpec describes one Lambda Labs instance type (the "gpu_<N>x_<gpu>"
// SKU). Lambda's catalog is much smaller and tighter than Vast's
// marketplace -- fixed offerings, fixed prices, with availability
// changing by region rather than by host. Verified via probe of
// /api/v1/instance-types on 2026-06.
//
// The catalog here is the SUBSET we offer for the iplane class
// taxonomy (small / medium / large / xlarge). Lambda has more (the
// 8x H100 SXM5 at $32/hr, the 8x B200 at $53/hr); operators who
// want those pass --gpu-sku explicitly.
type SKUSpec struct {
	// Name is the Lambda Labs instance_type_name (e.g. "gpu_1x_a10").
	Name string
	// DisplayName is the human-readable form Lambda's docs use.
	DisplayName string
	// VRAMGb is GPU memory per card.
	VRAMGb int
	// GPUCount is the number of GPUs on the instance.
	GPUCount int
	// PriceUSDPerHour is the on-demand price at cataloging.
	// Authoritative price comes back on each /instance-types call.
	PriceUSDPerHour float64
	// Family maps this SKU onto the cross-provider fabric catalog.
	// Lambda names the form factor in the instance type itself
	// ("gpu_1x_a100_sxm4" vs "gpu_1x_h100_pcie"), so the declared tier
	// reads straight off the SKU the same way RunPod's does. Lambda
	// exposes no fabric measurement, so DECLARED is the only tier here.
	Family fabric.Family
}

// skus is the catalog the resolver iterates. Sorted by VRAM tier
// (small / medium / large / xlarge) then by price within tier.
// Lambda's pricing is fixed (unlike Vast's marketplace), so the
// "cheapest-first" ordering is deterministic at catalog time.
//
// SKUs omitted from this curated list (operator can still request
// via --gpu-sku):
//   - gpu_4x_a100, gpu_8x_a100 (multi-GPU; iplane v0.2 is single-GPU
//     per replica)
//   - gpu_4x_h100_*, gpu_8x_h100_* (same)
//   - gpu_*x_b200_* (same; also very expensive)
//   - gpu_8x_v100_n (older arch; iplane doesn't optimize for V100)
var skus = []SKUSpec{
	// Small (~24 GB VRAM): A10 is the cheap entry.
	{Name: "gpu_1x_a10", DisplayName: "1x A10 (24 GB)", VRAMGb: 24, GPUCount: 1, PriceUSDPerHour: 1.29, Family: fabric.FamilyA10},
	// Medium (~48 GB VRAM): A6000 isn't in Lambda's catalog;
	// closest equivalent is the A100 PCIE at 40 GB.
	{Name: "gpu_1x_a100", DisplayName: "1x A100 (40 GB PCIE)", VRAMGb: 40, GPUCount: 1, PriceUSDPerHour: 1.29, Family: fabric.FamilyA100PCIe},
	// Large (~80 GB VRAM): A100 SXM4 / H100 PCIE.
	{Name: "gpu_1x_a100_sxm4", DisplayName: "1x A100 (80 GB SXM4)", VRAMGb: 80, GPUCount: 1, PriceUSDPerHour: 1.99, Family: fabric.FamilyA100SXM},
	{Name: "gpu_1x_h100_pcie", DisplayName: "1x H100 (80 GB PCIE)", VRAMGb: 80, GPUCount: 1, PriceUSDPerHour: 3.29, Family: fabric.FamilyH100PCIe},
	{Name: "gpu_1x_h100_sxm5", DisplayName: "1x H100 (80 GB SXM5)", VRAMGb: 80, GPUCount: 1, PriceUSDPerHour: 4.29, Family: fabric.FamilyH100SXM},
	// XL (>=96 GB): GH200 superchip.
	{Name: "gpu_1x_gh200", DisplayName: "1x GH200 (96 GB)", VRAMGb: 96, GPUCount: 1, PriceUSDPerHour: 2.29, Family: fabric.FamilyGH200},
	{Name: "gpu_1x_b200_sxm6", DisplayName: "1x B200 (180 GB SXM6)", VRAMGb: 180, GPUCount: 1, PriceUSDPerHour: 6.99, Family: fabric.FamilyB200},
}

// MaxSKUsPerRequest caps the SKUs the resolver will try when no
// operator-supplied --gpu-sku narrows the search. The cap and its
// price-escalation rationale are shared (skucatalog.MaxResults). Lambda is the
// provider that uses the cap least: it doesn't iterate, since each Spawn knows
// its exact instance type before calling /instance-operations/launch. The cap
// exists here for forward-compat when a future retry path iterates the
// resolver's output.
const MaxSKUsPerRequest = skucatalog.MaxResults

// catalogEntries projects the Lambda catalog onto the shared resolver's fact
// set. Lambda is the one provider whose rows describe a whole instance rather
// than a card, so GPUCount is populated here and nowhere else, and it is the
// only catalog publishing no system-RAM figure. A zero SystemRAMGb means
// min_ram_gb cannot narrow this catalog, which is the resolver's rule for a
// sizing fact nobody stated (see #283 for whether that is the right answer).
func catalogEntries() []skucatalog.Entry {
	out := make([]skucatalog.Entry, 0, len(skus))
	for _, sku := range skus {
		out = append(out, skucatalog.Entry{
			Token:           sku.Name,
			VRAMGb:          sku.VRAMGb,
			GPUCount:        sku.GPUCount,
			PriceUSDPerHour: sku.PriceUSDPerHour,
			Family:          sku.Family,
		})
	}
	return out
}

// MatchSKUs is the per-provider resolver. Given a
// ResourceRequirements, returns the ordered list of Lambda instance
// type names that satisfy every numeric constraint, cheapest first,
// capped at MaxSKUsPerRequest.
//
// FabricDeclared, because Lambda names the form factor in the instance type
// itself ("gpu_1x_a100_sxm4" vs "gpu_1x_h100_pcie") and reports no
// measurement, so the declared tier reads straight off the SKU the way
// RunPod's does.
//
// Returns an empty slice if no SKU in the catalog satisfies the
// constraints; Spawn surfaces this as "no matching SKU" rather
// than silently passing nothing.
func MatchSKUs(reqs *provisionerv1.ResourceRequirements) []string {
	return skucatalog.Match(catalogEntries(), reqs, skucatalog.FabricDeclared)
}

// LookupSKU returns the catalog entry for a Lambda Labs instance
// type name, accepting either the canonical form ("gpu_1x_a10") or
// the rare typo form with spaces ("gpu 1x a10"). Returns nil for
// types not in our curated list -- typical for operator-supplied
// --gpu-sku that hits Lambda's broader catalog.
func LookupSKU(name string) *SKUSpec {
	norm := strings.ReplaceAll(strings.TrimSpace(name), " ", "_")
	for i := range skus {
		if skus[i].Name == norm {
			return &skus[i]
		}
	}
	return nil
}

// classifySKU returns the class a catalogued SKU belongs to. Unknown SKUs
// return "" -- the operator-supplied --gpu-sku case where we have no opinion
// about classification.
func classifySKU(name string) string {
	sku := LookupSKU(name)
	if sku == nil {
		return ""
	}
	return provisioners.ClassifyByVRAM(sku.VRAMGb)
}

// isActiveProviderState reports whether a Lambda Labs instance
// status counts as "the instance is up and idempotency-adoptable."
//
// Lambda's status values (verified via probe):
//   - "booting"  -> pod provisioning, not yet SSH-reachable.
//   - "active"   -> SSH up, IP assigned.
//   - "unhealthy"-> degraded but still rented.
//   - "terminating" / "terminated" -> teardown in progress / done.
//
// "booting" + "active" + "unhealthy" count as active for adoption
// purposes: the rented contract exists, charges are accruing.
func isActiveProviderState(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "booting", "active", "unhealthy":
		return true
	}
	return false
}

// stampFabric fills the fabric fields on a Hardware record for a Lambda
// instance type. Lambda reports no measurement, so this is always the
// declared tier read off the SKU's form factor; an instance type outside our
// curated catalog resolves to UNKNOWN rather than to a guess.
func stampFabric(hw *provisionerv1.Hardware, instanceTypeName string) {
	var family fabric.Family
	if spec := LookupSKU(instanceTypeName); spec != nil {
		family = spec.Family
	}
	fabric.Stamp(hw, fabric.Observation{Family: family})
}
