package lambdalabs

import (
	"sort"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
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
	{Name: "gpu_1x_a10", DisplayName: "1x A10 (24 GB)", VRAMGb: 24, GPUCount: 1, PriceUSDPerHour: 1.29},
	// Medium (~48 GB VRAM): A6000 isn't in Lambda's catalog;
	// closest equivalent is the A100 PCIE at 40 GB.
	{Name: "gpu_1x_a100", DisplayName: "1x A100 (40 GB PCIE)", VRAMGb: 40, GPUCount: 1, PriceUSDPerHour: 1.29},
	// Large (~80 GB VRAM): A100 SXM4 / H100 PCIE.
	{Name: "gpu_1x_a100_sxm4", DisplayName: "1x A100 (80 GB SXM4)", VRAMGb: 80, GPUCount: 1, PriceUSDPerHour: 1.99},
	{Name: "gpu_1x_h100_pcie", DisplayName: "1x H100 (80 GB PCIE)", VRAMGb: 80, GPUCount: 1, PriceUSDPerHour: 3.29},
	{Name: "gpu_1x_h100_sxm5", DisplayName: "1x H100 (80 GB SXM5)", VRAMGb: 80, GPUCount: 1, PriceUSDPerHour: 4.29},
	// XL (>=96 GB): GH200 superchip.
	{Name: "gpu_1x_gh200", DisplayName: "1x GH200 (96 GB)", VRAMGb: 96, GPUCount: 1, PriceUSDPerHour: 2.29},
	{Name: "gpu_1x_b200_sxm6", DisplayName: "1x B200 (180 GB SXM6)", VRAMGb: 180, GPUCount: 1, PriceUSDPerHour: 6.99},
}

// MaxSKUsPerRequest caps the SKUs the resolver will try when no
// operator-supplied --gpu-sku narrows the search. Unlike Vast,
// Lambda doesn't iterate -- each Spawn knows its exact instance
// type before calling /instance-operations/launch. The cap exists
// for forward-compat when a future retry path iterates the
// resolver's output.
const MaxSKUsPerRequest = 5

// MatchSKUs is the per-provider resolver. Given a
// ResourceRequirements, returns the ordered list of Lambda instance
// type names that satisfy every numeric constraint, cheapest first,
// capped at MaxSKUsPerRequest.
//
// Returns an empty slice if no SKU in the catalog satisfies the
// constraints; Spawn surfaces this as "no matching SKU" rather
// than silently passing nothing.
func MatchSKUs(reqs *provisionerv1.ResourceRequirements) []string {
	if reqs == nil {
		return nil
	}
	var matches []SKUSpec
	for _, sku := range skus {
		if sku.VRAMGb < int(reqs.GetMinVramGb()) {
			continue
		}
		if reqs.GetGpuCount() > 0 && sku.GPUCount < int(reqs.GetGpuCount()) {
			continue
		}
		matches = append(matches, sku)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].PriceUSDPerHour < matches[j].PriceUSDPerHour
	})
	if len(matches) > MaxSKUsPerRequest {
		matches = matches[:MaxSKUsPerRequest]
	}
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.Name
	}
	return out
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

// classifySKU returns the class a SKU belongs to, derived from
// VRAM. Unknown SKUs return "" -- the operator-supplied --gpu-sku
// case where we have no opinion about classification.
func classifySKU(name string) string {
	sku := LookupSKU(name)
	if sku == nil {
		return ""
	}
	switch {
	case sku.VRAMGb >= 96:
		return provisioners.GPUClassXLarge
	case sku.VRAMGb >= 80:
		return provisioners.GPUClassLarge
	case sku.VRAMGb >= 40:
		return provisioners.GPUClassMedium
	default:
		return provisioners.GPUClassSmall
	}
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
