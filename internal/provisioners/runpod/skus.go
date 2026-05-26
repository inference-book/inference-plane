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
// Each GpuTypeID must be an exact enum value in RunPod's REST schema
// (the validator on POST /pods rejects anything outside its enum).
// Confirm current values against:
//
//	curl -X GET https://rest.runpod.io/v1/gpus -H "Authorization: Bearer $RUNPOD_API_KEY"
//
// RunPod periodically retires SKUs (e.g., the A100 40GB PCIe was
// dropped from the enum sometime before 2026-05) and adds new ones
// (B200, H200 variants, Blackwell PRO 6000 line). A wrong entry here
// surfaces as a 400 schema validation rejection with the full enum
// list in the problems field, which is enough to update the catalog.
var skus = []SKUSpec{
	// Small (>=24 GB VRAM): consumer + entry datacenter.
	{GpuTypeID: "NVIDIA GeForce RTX 4090", VRAMGb: 24, DefaultSystemRAMGb: 16, DefaultDiskGb: 20, PriceUSDPerHour: 0.39},
	{GpuTypeID: "NVIDIA RTX A5000", VRAMGb: 24, DefaultSystemRAMGb: 24, DefaultDiskGb: 20, PriceUSDPerHour: 0.36},
	{GpuTypeID: "NVIDIA L4", VRAMGb: 24, DefaultSystemRAMGb: 24, DefaultDiskGb: 20, PriceUSDPerHour: 0.43},
	{GpuTypeID: "NVIDIA A30", VRAMGb: 24, DefaultSystemRAMGb: 32, DefaultDiskGb: 20, PriceUSDPerHour: 0.45},
	{GpuTypeID: "NVIDIA GeForce RTX 5090", VRAMGb: 32, DefaultSystemRAMGb: 24, DefaultDiskGb: 20, PriceUSDPerHour: 0.69},

	// Medium (>=40 GB VRAM): workstation / mid-datacenter.
	{GpuTypeID: "NVIDIA A40", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.39},
	{GpuTypeID: "NVIDIA L40", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.69},
	{GpuTypeID: "NVIDIA L40S", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.79},
	{GpuTypeID: "NVIDIA RTX A6000", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.79},
	{GpuTypeID: "NVIDIA RTX 6000 Ada Generation", VRAMGb: 48, DefaultSystemRAMGb: 48, DefaultDiskGb: 40, PriceUSDPerHour: 0.99},

	// Large (>=80 GB VRAM): 70B-class inference territory.
	{GpuTypeID: "NVIDIA A100 80GB PCIe", VRAMGb: 80, DefaultSystemRAMGb: 96, DefaultDiskGb: 60, PriceUSDPerHour: 1.69},
	{GpuTypeID: "NVIDIA A100-SXM4-80GB", VRAMGb: 80, DefaultSystemRAMGb: 96, DefaultDiskGb: 60, PriceUSDPerHour: 1.79},
	{GpuTypeID: "NVIDIA H100 PCIe", VRAMGb: 80, DefaultSystemRAMGb: 128, DefaultDiskGb: 60, PriceUSDPerHour: 2.39},
	{GpuTypeID: "NVIDIA H100 80GB HBM3", VRAMGb: 80, DefaultSystemRAMGb: 128, DefaultDiskGb: 60, PriceUSDPerHour: 2.49},

	// XL (>=94 GB VRAM): frontier / 400B-class multi-host.
	{GpuTypeID: "NVIDIA H100 NVL", VRAMGb: 94, DefaultSystemRAMGb: 128, DefaultDiskGb: 100, PriceUSDPerHour: 2.99},
	{GpuTypeID: "NVIDIA H200", VRAMGb: 141, DefaultSystemRAMGb: 192, DefaultDiskGb: 100, PriceUSDPerHour: 3.99},
	{GpuTypeID: "NVIDIA B200", VRAMGb: 192, DefaultSystemRAMGb: 256, DefaultDiskGb: 100, PriceUSDPerHour: 5.99},
}

// Class-to-constraint-defaults lives in the service layer
// (internal/provisioners/service.go classDefaults) -- one mapping
// shared across providers means "class=small" resolves to the same
// numeric requirements on RunPod, Lambda, AWS, anywhere. The runpod
// adapter only sees expanded constraints.

// MaxSKUsPerRequest caps the number of gpuTypeIds we send to RunPod on
// a single create. Two reasons to cap:
//
//   - Class shorthand has no upper bound today (class=small expands to
//     min_vram_gb=24 with no max). Without a cap, every SKU with
//     VRAM>=24 enters the candidate list -- including B200 at 192 GB.
//     An operator who asked for "small" should not silently land on a
//     frontier GPU because the cheap tier is exhausted; the price would
//     be 10x higher than expected.
//
//   - Some RunPod accounts are restricted from provisioning specific
//     SKUs (B200, H200, H100 NVL typically require approval). Including
//     restricted SKUs in gpuTypeIds can trigger account-level 401s
//     across the whole request rather than RunPod just skipping the
//     restricted entries.
//
// Capping at top-5 cheapest preserves real fallback (RunPod will try
// each in order if the cheapest is unavailable) without exposing the
// caller to large price-tier jumps. Operators who want a strict ceiling
// pass --gpu-sku for an explicit single-SKU request; a future
// max_vram_gb constraint would let class shorthand carry a real upper
// bound (see ROADMAP for the eventual fix).
const MaxSKUsPerRequest = 5

// MatchSKUs is the per-provider resolver in the (a) constraints /
// (b) resolver / (c) executor model. Given a ResourceRequirements,
// it returns the ordered list of gpuTypeIds that satisfy every
// numeric constraint, cheapest first, capped at MaxSKUsPerRequest.
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
	if len(matches) > MaxSKUsPerRequest {
		matches = matches[:MaxSKUsPerRequest]
	}
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
