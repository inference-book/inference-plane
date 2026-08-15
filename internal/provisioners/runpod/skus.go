package runpod

import (
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
	"github.com/inference-book/inference-plane/internal/provisioners/skucatalog"
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

	// Family maps this SKU onto the cross-provider fabric catalog.
	//
	// RunPod exposes no interconnect field anywhere in its API, so the form
	// factor baked into the gpuTypeId string is the only signal available:
	// "NVIDIA A100-SXM4-80GB" is on an NVSwitch mesh, "NVIDIA A100 80GB PCIe"
	// is on a PCIe root that may or may not carry a bridge. Every datacenter
	// part names its form factor, which is what makes this tractable.
	// Verified against the live gpuTypes catalog 2026-08-09.
	Family fabric.Family
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
	{GpuTypeID: "NVIDIA GeForce RTX 4090", VRAMGb: 24, DefaultSystemRAMGb: 16, DefaultDiskGb: 20, PriceUSDPerHour: 0.39, Family: fabric.FamilyRTX4090},
	{GpuTypeID: "NVIDIA RTX A5000", VRAMGb: 24, DefaultSystemRAMGb: 24, DefaultDiskGb: 20, PriceUSDPerHour: 0.36, Family: fabric.FamilyA5000},
	{GpuTypeID: "NVIDIA L4", VRAMGb: 24, DefaultSystemRAMGb: 24, DefaultDiskGb: 20, PriceUSDPerHour: 0.43, Family: fabric.FamilyL4},
	// "NVIDIA A30" was retired from RunPod's REST enum (confirmed 2026-07-27:
	// a create with it in gpuTypeIds 400s with the full enum, A30 absent).
	// Any min_vram_gb<=24 request included it in the top-5 and failed whole.
	{GpuTypeID: "NVIDIA GeForce RTX 5090", VRAMGb: 32, DefaultSystemRAMGb: 24, DefaultDiskGb: 20, PriceUSDPerHour: 0.69, Family: fabric.FamilyRTX5090},

	// Medium (>=40 GB VRAM): workstation / mid-datacenter.
	{GpuTypeID: "NVIDIA A40", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.39, Family: fabric.FamilyA40},
	{GpuTypeID: "NVIDIA L40", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.69, Family: fabric.FamilyL40},
	{GpuTypeID: "NVIDIA L40S", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.79, Family: fabric.FamilyL40S},
	{GpuTypeID: "NVIDIA RTX A6000", VRAMGb: 48, DefaultSystemRAMGb: 32, DefaultDiskGb: 40, PriceUSDPerHour: 0.79, Family: fabric.FamilyA6000},
	{GpuTypeID: "NVIDIA RTX 6000 Ada Generation", VRAMGb: 48, DefaultSystemRAMGb: 48, DefaultDiskGb: 40, PriceUSDPerHour: 0.99, Family: fabric.FamilyRTX6000Ada},

	// Large (>=80 GB VRAM): 70B-class inference territory.
	{GpuTypeID: "NVIDIA A100 80GB PCIe", VRAMGb: 80, DefaultSystemRAMGb: 96, DefaultDiskGb: 60, PriceUSDPerHour: 1.69, Family: fabric.FamilyA100PCIe},
	{GpuTypeID: "NVIDIA A100-SXM4-80GB", VRAMGb: 80, DefaultSystemRAMGb: 96, DefaultDiskGb: 60, PriceUSDPerHour: 1.79, Family: fabric.FamilyA100SXM},
	{GpuTypeID: "NVIDIA H100 PCIe", VRAMGb: 80, DefaultSystemRAMGb: 128, DefaultDiskGb: 60, PriceUSDPerHour: 2.39, Family: fabric.FamilyH100PCIe},
	{GpuTypeID: "NVIDIA H100 80GB HBM3", VRAMGb: 80, DefaultSystemRAMGb: 128, DefaultDiskGb: 60, PriceUSDPerHour: 2.49, Family: fabric.FamilyH100SXM},

	// XL (>=94 GB VRAM): frontier / 400B-class multi-host.
	{GpuTypeID: "NVIDIA H100 NVL", VRAMGb: 94, DefaultSystemRAMGb: 128, DefaultDiskGb: 100, PriceUSDPerHour: 2.99, Family: fabric.FamilyH100NVL},
	{GpuTypeID: "NVIDIA H200", VRAMGb: 141, DefaultSystemRAMGb: 192, DefaultDiskGb: 100, PriceUSDPerHour: 3.99, Family: fabric.FamilyH200SXM},
	{GpuTypeID: "NVIDIA B200", VRAMGb: 192, DefaultSystemRAMGb: 256, DefaultDiskGb: 100, PriceUSDPerHour: 5.99, Family: fabric.FamilyB200},
}

// Class-to-constraint-defaults lives in the service layer
// (internal/provisioners/service.go classDefaults) -- one mapping
// shared across providers means "class=small" resolves to the same
// numeric requirements on RunPod, Lambda, AWS, anywhere. The runpod
// adapter only sees expanded constraints.

// MaxSKUsPerRequest caps the number of gpuTypeIds we send to RunPod on
// a single create. The cap and its price-escalation rationale are shared
// (skucatalog.MaxResults); what is RunPod-specific is why the list has more
// than one entry at all, and one extra reason to keep it short:
//
//   - RunPod takes the whole list on one create and tries the entries in
//     order (gpuTypePriority=availability or custom controlling the policy),
//     so the tail of the list is real fallback rather than a second call.
//
//   - Some RunPod accounts are restricted from provisioning specific
//     SKUs (B200, H200, H100 NVL typically require approval). Including
//     restricted SKUs in gpuTypeIds can trigger account-level 401s
//     across the whole request rather than RunPod just skipping the
//     restricted entries.
//
// Operators who want a strict ceiling pass --gpu-sku for an explicit
// single-SKU request; a future max_vram_gb constraint would let class
// shorthand carry a real upper bound (see ROADMAP for the eventual fix).
const MaxSKUsPerRequest = skucatalog.MaxResults

// catalogEntries projects the RunPod catalog onto the shared resolver's fact
// set. DefaultDiskGb is deliberately absent: disk is an independent create
// param sized from min_disk_gb by the deployer, so it is not a fact a catalog
// row bounds and must never filter (#281).
func catalogEntries() []skucatalog.Entry {
	out := make([]skucatalog.Entry, 0, len(skus))
	for _, sku := range skus {
		out = append(out, skucatalog.Entry{
			Token:           sku.GpuTypeID,
			VRAMGb:          sku.VRAMGb,
			SystemRAMGb:     sku.DefaultSystemRAMGb,
			PriceUSDPerHour: sku.PriceUSDPerHour,
			Family:          sku.Family,
		})
	}
	return out
}

// MatchSKUs is the per-provider resolver in the (a) constraints /
// (b) resolver / (c) executor model. Given a ResourceRequirements,
// it returns the ordered list of gpuTypeIds that satisfy every
// numeric constraint, cheapest first, capped at MaxSKUsPerRequest.
//
// The returned slice is the gpuTypeIds value the Spawn call passes
// to RunPod's POST /pods.
//
// FabricDeclared, because RunPod supplies no interconnect measurement
// anywhere in its API. Every fabric verdict here is the tier declared by the
// form factor in the SKU name, so bridge-capable PCIe parts resolve to UNKNOWN
// and fail closed rather than being rented to find out.
//
// Returns an empty slice if no SKU in the catalog satisfies the
// constraints; the caller should surface this as "no matching SKU"
// rather than silently passing an empty list to RunPod.
func MatchSKUs(reqs *provisionerv1.ResourceRequirements) []string {
	return skucatalog.Match(catalogEntries(), reqs, skucatalog.FabricDeclared)
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

// classifySKU returns the class a catalogued SKU belongs to. Unknown SKUs
// return "" -- the operator-supplied --gpu-sku case where we have no opinion
// about classification.
func classifySKU(gpuTypeID string) string {
	sku := LookupSKU(gpuTypeID)
	if sku == nil {
		return ""
	}
	return provisioners.ClassifyByVRAM(sku.VRAMGb)
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

// stampFabric fills the fabric fields on a Hardware record for a resolved
// RunPod SKU. RunPod reports no measurement, so this is always the declared
// tier, and an operator-supplied --gpu-sku outside our catalog resolves to
// UNKNOWN rather than to a guess.
func stampFabric(hw *provisionerv1.Hardware, gpuTypeID string) {
	var family fabric.Family
	if spec := LookupSKU(gpuTypeID); spec != nil {
		family = spec.Family
	}
	fabric.Stamp(hw, fabric.Observation{Family: family})
}
