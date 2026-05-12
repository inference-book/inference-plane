package runpod

import (
	"sort"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// SKUSpec describes what a RunPod gpuTypeId actually delivers: VRAM,
// the system RAM that comes with one GPU on RunPod's default pod
// shape, and a rough price tier (USD/hr at the cheapest cloud type).
//
// Verify against the live API with:
//
//	curl -X GET https://rest.runpod.io/v1/gpus -H "Authorization: Bearer $RUNPOD_API_KEY"
//
// Or the GraphQL fallback:
//
//	curl -X POST https://api.runpod.io/graphql \
//	  -H "Authorization: Bearer $RUNPOD_API_KEY" \
//	  -H "Content-Type: application/json" \
//	  -d '{"query": "query { gpuTypes { id displayName memoryInGb securePrice communityPrice } }"}'
//
// These are pre-Spawn estimates; the actual price (and the actual VRAM
// allocation) is reported by the provider in the Spawn response and
// recorded in the iplane state file.
type SKUSpec struct {
	GpuTypeID          string  // RunPod's literal gpuTypeId string
	VRAMGb             int     // GPU memory per card
	DefaultSystemRAMGb int     // system RAM for a typical single-GPU pod with this SKU
	DefaultDiskGb      int     // typical container-disk default for this tier
	PriceUSDPerHour    float64 // cheapest cloud tier price; for ordering, not authoritative
}

// skus is the catalog the resolver iterates. Order in the slice is the
// fallback order RunPod tries when we pass gpuTypeIds as an array --
// availability-first if we leave gpuTypePriority on its default. The
// resolver re-sorts the matching subset by price before emitting the
// final list.
//
// Adding a SKU: append to this slice. Do not reuse memoryInGb-1
// "almost large" tiers (e.g., L40S at 48GB) without thinking about
// which class they should expand into; the constraint check is
// authoritative and a class-table refresh is optional.
var skus = []SKUSpec{
	// Consumer / small
	{GpuTypeID: "NVIDIA GeForce RTX 4090", VRAMGb: 24, DefaultSystemRAMGb: 16, DefaultDiskGb: 20, PriceUSDPerHour: 0.39},
	{GpuTypeID: "NVIDIA GeForce RTX 5090", VRAMGb: 32, DefaultSystemRAMGb: 24, DefaultDiskGb: 20, PriceUSDPerHour: 0.69},

	// Workstation / medium
	{GpuTypeID: "NVIDIA RTX A6000", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.79},
	{GpuTypeID: "NVIDIA A100 40GB PCIe", VRAMGb: 40, DefaultSystemRAMGb: 48, DefaultDiskGb: 40, PriceUSDPerHour: 1.19},

	// Data-center / large
	{GpuTypeID: "NVIDIA A100 80GB PCIe", VRAMGb: 80, DefaultSystemRAMGb: 96, DefaultDiskGb: 60, PriceUSDPerHour: 1.69},
	{GpuTypeID: "NVIDIA A100-SXM4-80GB", VRAMGb: 80, DefaultSystemRAMGb: 96, DefaultDiskGb: 60, PriceUSDPerHour: 1.79},
	{GpuTypeID: "NVIDIA H100 80GB HBM3", VRAMGb: 80, DefaultSystemRAMGb: 128, DefaultDiskGb: 60, PriceUSDPerHour: 2.49},

	// XL / frontier
	{GpuTypeID: "NVIDIA H100 NVL", VRAMGb: 94, DefaultSystemRAMGb: 128, DefaultDiskGb: 100, PriceUSDPerHour: 2.99},
}

// Class-to-constraint-defaults lives in the service layer
// (internal/provisioners/service.go classDefaults) -- one mapping
// shared across providers means "class=small" resolves to the same
// numeric requirements on RunPod, Lambda, AWS, anywhere. The runpod
// adapter only sees expanded constraints.

// MatchSKUs is the per-provider resolver in the (a) constraints /
// (b) resolver / (c) executor model. Given a ResourceRequirements,
// it returns the ordered list of gpuTypeIds that satisfy every
// numeric constraint, cheapest first.
//
// The returned slice is the gpuTypeIds value the Spawn call passes
// to RunPod's POST /pods -- RunPod tries them in order (with
// gpuTypePriority=availability or custom controlling the policy).
//
// Returns an empty slice if no SKU in the catalog satisfies the
// constraints; the caller should surface this as "no matching SKU"
// rather than silently passing an empty list to RunPod.
func MatchSKUs(reqs *provisionerv1.ResourceRequirements) []string {
	if reqs == nil {
		return nil
	}
	var matches []SKUSpec
	for _, sku := range skus {
		if sku.VRAMGb < int(reqs.GetMinVramGb()) {
			continue
		}
		if int(reqs.GetMinDiskGb()) > 0 && sku.DefaultDiskGb < int(reqs.GetMinDiskGb()) {
			continue
		}
		if int(reqs.GetMinRamGb()) > 0 && sku.DefaultSystemRAMGb < int(reqs.GetMinRamGb()) {
			continue
		}
		matches = append(matches, sku)
	}
	sort.SliceStable(matches, func(i, j int) bool {
		return matches[i].PriceUSDPerHour < matches[j].PriceUSDPerHour
	})
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.GpuTypeID
	}
	return out
}

// LookupSKU returns the catalog entry for a known gpuTypeId, or nil
// if the SKU is operator-supplied and not in our table. The runpod
// Provider uses this on the Describe path to surface a class label
// when the SKU is known.
func LookupSKU(gpuTypeID string) *SKUSpec {
	for i := range skus {
		if skus[i].GpuTypeID == gpuTypeID {
			return &skus[i]
		}
	}
	return nil
}

// classifySKU returns the class a SKU belongs to, derived from its
// VRAM (not a hardcoded reverse table). An RTX 4090 at 24 GB is
// "small" because it falls in the [24, 40) VRAM band, full stop.
// Unknown SKUs return "" -- the operator-supplied --gpu-sku case
// where we have no opinion about classification.
func classifySKU(gpuTypeID string) string {
	sku := LookupSKU(gpuTypeID)
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

// knownClasses lists class shorthand keys for error messages.
func knownClasses() []string {
	return []string{
		provisioners.GPUClassSmall,
		provisioners.GPUClassMedium,
		provisioners.GPUClassLarge,
		provisioners.GPUClassXLarge,
	}
}

// isActiveProviderState reports whether a RunPod desiredStatus counts
// as "the instance is up and idempotency-adoptable." See the
// ActiveStateChecker discussion in internal/provisioners/service.go
// for why this lives in the adapter package rather than centrally.
func isActiveProviderState(state string) bool {
	switch strings.ToUpper(state) {
	case "CREATED", "RUNNING", "RESTARTING":
		return true
	}
	return false
}
