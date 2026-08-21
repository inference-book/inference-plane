package vast

import (
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
	"github.com/inference-book/inference-plane/internal/provisioners/skucatalog"
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
	GpuName            string  // iplane's SKU token (underscored form); unique in this catalog
	DisplayName        string  // human-readable name; matches the gpu_name returned by list/get
	VRAMGb             int     // GPU memory per card
	DefaultSystemRAMGb int     // system RAM for a typical single-GPU host with this SKU
	PriceUSDPerHour    float64 // marketplace floor at cataloging; for ordering, not authoritative

	// WireName is the gpu_name Vast actually filters on, when it differs from
	// GpuName. Empty means they are the same.
	//
	// It exists because Vast sells physically different cards under one
	// gpu_name: "A100 SXM4" is both the 40 GB and the 80 GB part. One catalog
	// row per gpu_name could therefore describe only one of them, and #243
	// resolved that collision by treating the row's VRAM as a floor, which
	// correctly stopped a 40 GB card being handed to someone who asked for
	// 80 GB but also made the 40 GB part unrequestable. Splitting into two
	// rows that share a WireName lets both be asked for by name.
	WireName string

	// VRAMMaxGb bounds the card's memory from above, in GB. 0 means unbounded,
	// which is the right default for a row that is the only one for its
	// gpu_name.
	//
	// Set it on variant rows, because there the upper bound is the whole point:
	// asking for the 40 GB part and being handed the 80 GB one is a silently
	// more expensive rental, and in an A/B where both arms must carry the same
	// card it is a confound rather than a free upgrade. This follows the same
	// reasoning as the floor in offerVRAMFloorMB, in the other direction:
	// naming a SKU is asking for that card, not for something sharing its
	// marketing name.
	VRAMMaxGb int

	// Family maps this SKU onto the cross-provider fabric catalog.
	//
	// Vast is the one provider that MEASURES the fabric (bw_nvlink on each
	// offer), so unlike RunPod and Lambda this catalog entry is only a
	// pre-filter: it decides whether a family is worth searching, and the
	// per-offer reading decides the verdict. A "PCIE" SKU stays searchable
	// precisely because some of those hosts turn out to be bridged.
	Family fabric.Family
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
	{GpuName: "RTX_3090", DisplayName: "RTX 3090", VRAMGb: 24, DefaultSystemRAMGb: 32, PriceUSDPerHour: 0.20, Family: fabric.FamilyRTX3090},
	{GpuName: "RTX_4090", DisplayName: "RTX 4090", VRAMGb: 24, DefaultSystemRAMGb: 32, PriceUSDPerHour: 0.30, Family: fabric.FamilyRTX4090},
	{GpuName: "RTX_A5000", DisplayName: "RTX A5000", VRAMGb: 24, DefaultSystemRAMGb: 32, PriceUSDPerHour: 0.28, Family: fabric.FamilyA5000},
	{GpuName: "L4", DisplayName: "L4", VRAMGb: 24, DefaultSystemRAMGb: 32, PriceUSDPerHour: 0.40, Family: fabric.FamilyL4},
	{GpuName: "RTX_5090", DisplayName: "RTX 5090", VRAMGb: 32, DefaultSystemRAMGb: 32, PriceUSDPerHour: 0.55, Family: fabric.FamilyRTX5090},

	// Medium (>=40 GB VRAM): workstation / mid-datacenter.
	{GpuName: "A40", DisplayName: "A40", VRAMGb: 48, DefaultSystemRAMGb: 48, PriceUSDPerHour: 0.40, Family: fabric.FamilyA40},
	{GpuName: "L40", DisplayName: "L40", VRAMGb: 48, DefaultSystemRAMGb: 48, PriceUSDPerHour: 0.65, Family: fabric.FamilyL40},
	{GpuName: "L40S", DisplayName: "L40S", VRAMGb: 48, DefaultSystemRAMGb: 48, PriceUSDPerHour: 0.75, Family: fabric.FamilyL40S},
	{GpuName: "RTX_A6000", DisplayName: "RTX A6000", VRAMGb: 48, DefaultSystemRAMGb: 48, PriceUSDPerHour: 0.70, Family: fabric.FamilyA6000},
	{GpuName: "RTX_6000Ada", DisplayName: "RTX 6000 Ada", VRAMGb: 48, DefaultSystemRAMGb: 64, PriceUSDPerHour: 0.90, Family: fabric.FamilyRTX6000Ada},

	// A100 40 GB. Same wire gpu_name as the 80 GB rows below, distinguished by
	// the VRAM band. Cheaper, and at the time of cataloging (2026-08) the 40 GB
	// tier carried most of the healthy multi-GPU A100 capacity on the
	// marketplace while the 80 GB NVLink tier was nearly all broken or
	// low-reliability, so this is the tier a 4-GPU A100 request usually wants.
	{GpuName: "A100_PCIE_40GB", WireName: "A100_PCIE", DisplayName: "A100 PCIE", VRAMGb: 40, VRAMMaxGb: 40, DefaultSystemRAMGb: 96, PriceUSDPerHour: 0.80, Family: fabric.FamilyA100PCIe},
	{GpuName: "A100_SXM4_40GB", WireName: "A100_SXM4", DisplayName: "A100 SXM4", VRAMGb: 40, VRAMMaxGb: 40, DefaultSystemRAMGb: 96, PriceUSDPerHour: 0.90, Family: fabric.FamilyA100SXM},

	// Large (>=80 GB VRAM): 70B-class inference territory.
	{GpuName: "A100_PCIE", DisplayName: "A100 PCIE", VRAMGb: 80, DefaultSystemRAMGb: 128, PriceUSDPerHour: 1.20, Family: fabric.FamilyA100PCIe},
	{GpuName: "A100_SXM4", DisplayName: "A100 SXM4", VRAMGb: 80, DefaultSystemRAMGb: 128, PriceUSDPerHour: 1.30, Family: fabric.FamilyA100SXM},
	{GpuName: "H100_PCIE", DisplayName: "H100 PCIE", VRAMGb: 80, DefaultSystemRAMGb: 128, PriceUSDPerHour: 1.80, Family: fabric.FamilyH100PCIe},
	{GpuName: "H100_SXM", DisplayName: "H100 SXM", VRAMGb: 80, DefaultSystemRAMGb: 192, PriceUSDPerHour: 2.00, Family: fabric.FamilyH100SXM},

	// XL (>=94 GB VRAM): frontier / multi-tenant.
	{GpuName: "H100_NVL", DisplayName: "H100 NVL", VRAMGb: 94, DefaultSystemRAMGb: 192, PriceUSDPerHour: 2.30, Family: fabric.FamilyH100NVL},
	{GpuName: "H200", DisplayName: "H200", VRAMGb: 141, DefaultSystemRAMGb: 256, PriceUSDPerHour: 3.20, Family: fabric.FamilyH200SXM},
	{GpuName: "B200", DisplayName: "B200", VRAMGb: 192, DefaultSystemRAMGb: 256, PriceUSDPerHour: 4.80, Family: fabric.FamilyB200},
	// 288 GB per card, so eight of them hold 2304 GB. That is the
	// single-node route to a model that otherwise needs a cross-node pool:
	// Kimi K3 is 1560 GB at four bits and fits eight of nothing smaller.
	// Cataloged 2026-08-21 against live offers at $11.25/card-hr, 2x
	// through 8x, gpu_ram 275040 MiB.
	{GpuName: "B300", DisplayName: "B300", VRAMGb: 288, DefaultSystemRAMGb: 384, PriceUSDPerHour: 11.25, Family: fabric.FamilyB300},
}

// MaxSKUsPerRequest caps the SKUs the resolver will try when no
// operator-supplied --gpu-sku narrows the search. The cap and its
// price-escalation rationale are shared (skucatalog.MaxResults); what is Vast-
// specific is what the list costs to walk. Unlike RunPod, the vast adapter
// doesn't send the full list to one API call; instead MatchSKUs returns an
// ordered list and Spawn iterates: search for an offer of SKU[0], if none,
// search for SKU[1], etc. So each extra entry is another round trip to the
// marketplace, not a free fallback.
const MaxSKUsPerRequest = skucatalog.MaxResults

// catalogEntries projects the Vast catalog onto the shared resolver's fact
// set. Three fields deliberately do not cross over.
//
// DefaultSystemRAMGb is the newest omission (#283). Vast reports cpu_ram on
// every offer, so findOffer can push a RAM floor server-side and judge the
// actual host rather than a tier estimate. Once the real number is reachable,
// filtering on the estimate can only wrongly exclude, which is the same
// reasoning that removed the disk filter.
//
// There is no disk figure on SKUSpec at all, because disk is not a fact a
// catalog row bounds and must never filter (#281, #285). WireName and VRAMMaxGb
// are offer-level concerns: they disambiguate two physical cards sold under
// one gpu_name, which is settled by the VRAM floor and ceiling findOffer
// pushes into the search, not by which catalog rows are candidates.
func catalogEntries() []skucatalog.Entry {
	out := make([]skucatalog.Entry, 0, len(skus))
	for _, sku := range skus {
		out = append(out, skucatalog.Entry{
			Token:           sku.GpuName,
			VRAMGb:          sku.VRAMGb,
			PriceUSDPerHour: sku.PriceUSDPerHour,
			Family:          sku.Family,
		})
	}
	return out
}

// MatchSKUs is the per-provider resolver in the (a) constraints / (b)
// resolver / (c) executor model. Given a ResourceRequirements, it
// returns the ordered list of Vast.ai gpu_name tokens that satisfy
// every numeric constraint, cheapest first, capped at
// MaxSKUsPerRequest.
//
// FabricPrefilter, because Vast is the one provider that measures the fabric
// (bw_nvlink on each offer). The catalog only drops families that could never
// carry the requested fabric and leaves bridge-capable ones searchable, since
// findOffer pushes the bandwidth filter server-side and the reading on the
// picked offer is what actually decides. Dropping a "PCIE" SKU here would
// discard exactly the hosts worth having.
//
// Returns an empty slice if no SKU in the catalog satisfies the
// constraints; Spawn surfaces this as "no matching SKU" rather than
// silently passing nothing to Vast.ai's search.
func MatchSKUs(reqs *provisionerv1.ResourceRequirements) []string {
	return skucatalog.Match(catalogEntries(), reqs, skucatalog.FabricPrefilter)
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

// classifySKU returns the class a catalogued SKU belongs to. Unknown SKUs
// return "" -- the operator-supplied --gpu-sku case where we have no opinion
// about classification.
func classifySKU(gpuName string) string {
	sku := LookupSKU(gpuName)
	if sku == nil {
		return ""
	}
	return provisioners.ClassifyByVRAM(sku.VRAMGb)
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

// stampFabric fills the fabric fields on a Hardware record from the Vast SKU
// plus, when we have one, the host's measured bw_nvlink in gigaBYTES/sec.
//
// bwNvlink is a pointer so an absent field stays distinguishable from a
// measured zero; see offerSummary.BwNvlink for why that matters. Pass nil on
// paths where no reading is available and the declared tier answers instead.
func stampFabric(hw *provisionerv1.Hardware, gpuName string, bwNvlink *float64) {
	var family fabric.Family
	if spec := LookupSKU(gpuName); spec != nil {
		family = spec.Family
	}
	obs := fabric.Observation{Family: family}
	if bwNvlink != nil {
		obs.HasMeasurement = true
		obs.MeasuredGbps = fabric.GbpsFromGBps(*bwNvlink)
	}
	fabric.Stamp(hw, obs)
}

// CardCapacityBytes reports what one card of this SKU holds, or 0 when
// the catalog has no exact figure for it. Implements the optional
// provisioners.CardCapacityReporter capability, so a deploy can size a
// model against the card before renting it (#323).
func (p *Provider) CardCapacityBytes(sku string) int64 {
	spec := LookupSKU(sku)
	if spec == nil {
		return 0
	}
	return skucatalog.ExactVRAMBytes(spec.VRAMGb)
}
