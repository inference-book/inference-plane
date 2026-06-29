package vast

import (
	"sort"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// SKUSpec describes what a Vast.ai gpu_name actually delivers: VRAM
// (per card, GB), the system RAM typical for a host carrying one of
// these GPUs on the marketplace, and a rough price tier (USD/hr for
// a single-GPU on-demand offer at the time of cataloging).
//
// Verify against the live API with:
//
//	curl -X POST https://console.vast.ai/api/v0/bundles/ \
//	  -H "Authorization: Bearer $VAST_API_KEY" \
//	  -H "Content-Type: application/json" \
//	  -d '{"gpu_name":{"eq":"RTX_4090"},"verified":{"eq":true},"rentable":{"eq":true},"limit":5,"order":[["dph_total","asc"]]}'
//
// These are pre-Spawn estimates; the actual price (and the actual
// hardware spec) is reported in the offer record returned by the
// search call and again on the rented instance.
//
// GpuName format: Vast.ai's search filter uses underscores as token
// separators (e.g. "RTX_4090", not "RTX 4090"). The `actual_status`
// payload from list/get echoes "RTX 4090" (with the space) -- so the
// adapter normalizes back and forth at the boundary.
type SKUSpec struct {
	GpuName            string  // Vast.ai's gpu_name filter token (underscored form)
	DisplayName        string  // human-readable name; matches the gpu_name returned by list/get
	VRAMGb             int     // GPU memory per card
	DefaultSystemRAMGb int     // system RAM for a typical single-GPU host with this SKU
	DefaultDiskGb      int     // typical disk allocation default for this tier
	PriceUSDPerHour    float64 // marketplace floor at cataloging; for ordering, not authoritative
}

// skus is the catalog the resolver iterates. The slice is sorted
// roughly by VRAM tier (small/medium/large/xlarge) and within a tier
// by typical marketplace floor price; the resolver re-sorts the
// matching subset by price before emitting the final list.
//
// Vast.ai is a marketplace, not a fixed-catalog provider -- any SKU
// in this table may or may not have rentable capacity at any given
// moment. The Spawn path handles "no offer matched" as a discoverable
// failure ("no offer satisfies the constraints right now; try a
// different SKU or relax the requirement"). Unlike RunPod, we don't
// pass an ordered list to a single create call; we run search first,
// pick the cheapest offer that satisfies the constraints, and rent
// against that offer id.
var skus = []SKUSpec{
	// Small (>=24 GB VRAM): consumer + entry datacenter.
	{GpuName: "RTX_3090", DisplayName: "RTX 3090", VRAMGb: 24, DefaultSystemRAMGb: 32, DefaultDiskGb: 20, PriceUSDPerHour: 0.20},
	{GpuName: "RTX_4090", DisplayName: "RTX 4090", VRAMGb: 24, DefaultSystemRAMGb: 32, DefaultDiskGb: 20, PriceUSDPerHour: 0.30},
	{GpuName: "RTX_A5000", DisplayName: "RTX A5000", VRAMGb: 24, DefaultSystemRAMGb: 32, DefaultDiskGb: 20, PriceUSDPerHour: 0.28},
	{GpuName: "L4", DisplayName: "L4", VRAMGb: 24, DefaultSystemRAMGb: 32, DefaultDiskGb: 20, PriceUSDPerHour: 0.40},
	{GpuName: "RTX_5090", DisplayName: "RTX 5090", VRAMGb: 32, DefaultSystemRAMGb: 32, DefaultDiskGb: 20, PriceUSDPerHour: 0.55},

	// Medium (>=40 GB VRAM): workstation / mid-datacenter.
	{GpuName: "A40", DisplayName: "A40", VRAMGb: 48, DefaultSystemRAMGb: 48, DefaultDiskGb: 40, PriceUSDPerHour: 0.40},
	{GpuName: "L40", DisplayName: "L40", VRAMGb: 48, DefaultSystemRAMGb: 48, DefaultDiskGb: 40, PriceUSDPerHour: 0.65},
	{GpuName: "L40S", DisplayName: "L40S", VRAMGb: 48, DefaultSystemRAMGb: 48, DefaultDiskGb: 40, PriceUSDPerHour: 0.75},
	{GpuName: "RTX_A6000", DisplayName: "RTX A6000", VRAMGb: 48, DefaultSystemRAMGb: 48, DefaultDiskGb: 40, PriceUSDPerHour: 0.70},
	{GpuName: "RTX_6000Ada", DisplayName: "RTX 6000 Ada", VRAMGb: 48, DefaultSystemRAMGb: 64, DefaultDiskGb: 40, PriceUSDPerHour: 0.90},

	// Large (>=80 GB VRAM): 70B-class inference territory.
	{GpuName: "A100_PCIE", DisplayName: "A100 PCIE", VRAMGb: 80, DefaultSystemRAMGb: 128, DefaultDiskGb: 60, PriceUSDPerHour: 1.20},
	{GpuName: "A100_SXM4", DisplayName: "A100 SXM4", VRAMGb: 80, DefaultSystemRAMGb: 128, DefaultDiskGb: 60, PriceUSDPerHour: 1.30},
	{GpuName: "H100_PCIE", DisplayName: "H100 PCIE", VRAMGb: 80, DefaultSystemRAMGb: 128, DefaultDiskGb: 60, PriceUSDPerHour: 1.80},
	{GpuName: "H100_SXM", DisplayName: "H100 SXM", VRAMGb: 80, DefaultSystemRAMGb: 192, DefaultDiskGb: 60, PriceUSDPerHour: 2.00},

	// XL (>=94 GB VRAM): frontier / multi-tenant.
	{GpuName: "H100_NVL", DisplayName: "H100 NVL", VRAMGb: 94, DefaultSystemRAMGb: 192, DefaultDiskGb: 100, PriceUSDPerHour: 2.30},
	{GpuName: "H200", DisplayName: "H200", VRAMGb: 141, DefaultSystemRAMGb: 256, DefaultDiskGb: 100, PriceUSDPerHour: 3.20},
	{GpuName: "B200", DisplayName: "B200", VRAMGb: 192, DefaultSystemRAMGb: 256, DefaultDiskGb: 100, PriceUSDPerHour: 4.80},
}

// MaxSKUsPerRequest caps the SKUs the resolver will try when no
// operator-supplied --gpu-sku narrows the search. Unlike RunPod, the
// vast adapter doesn't send the full list to one API call; instead
// MatchSKUs returns an ordered list and Spawn iterates: search for an
// offer of SKU[0], if none, search for SKU[1], etc. The cap keeps the
// fallback bounded so an "any small GPU" request doesn't silently
// climb into B200 territory after every cheap tier is empty.
const MaxSKUsPerRequest = 5

// MatchSKUs is the per-provider resolver in the (a) constraints / (b)
// resolver / (c) executor model. Given a ResourceRequirements, it
// returns the ordered list of Vast.ai gpu_name tokens that satisfy
// every numeric constraint, cheapest first, capped at
// MaxSKUsPerRequest.
//
// Returns an empty slice if no SKU in the catalog satisfies the
// constraints; Spawn surfaces this as "no matching SKU" rather than
// silently passing nothing to Vast.ai's search.
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
		out[i] = m.GpuName
	}
	return out
}

// LookupSKU returns the catalog entry for a known gpu_name, accepting
// either the underscored search token ("RTX_4090") or the display
// form ("RTX 4090") so Describe / List can normalize whatever Vast
// returns back into the catalog. Returns nil for SKUs not in our
// table -- typical for operator-supplied --gpu-sku.
func LookupSKU(gpuName string) *SKUSpec {
	norm := normalizeGpuName(gpuName)
	for i := range skus {
		if skus[i].GpuName == norm {
			return &skus[i]
		}
	}
	return nil
}

// normalizeGpuName converts a free-form gpu_name into the underscored
// catalog token. Vast.ai's search uses "RTX_4090"; its list/get
// response uses "RTX 4090". We unify on the underscored form.
func normalizeGpuName(s string) string {
	return strings.ReplaceAll(strings.TrimSpace(s), " ", "_")
}

// classifySKU returns the class a SKU belongs to, derived from its
// VRAM (not a hardcoded reverse table). An RTX 4090 at 24 GB is
// "small" because it falls in the [24, 40) VRAM band, full stop.
// Unknown SKUs return "" -- the operator-supplied --gpu-sku case
// where we have no opinion about classification.
func classifySKU(gpuName string) string {
	sku := LookupSKU(gpuName)
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

// isActiveProviderState reports whether a Vast.ai actual_status counts
// as "the instance is up and idempotency-adoptable." Vast.ai's state
// machine surfaces values like "loading" (image pulling),
// "running" (container up), "stopped" (paused but billed for disk),
// "exited" (container terminated), and "offline" (host unreachable).
//
// "loading" and "running" count as active for adoption purposes:
// the rented contract exists, charges are accruing, and a List+adopt
// recovery should re-attach to the local record rather than create a
// duplicate.
func isActiveProviderState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "loading", "running", "scheduling", "created":
		return true
	}
	return false
}
