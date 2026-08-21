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
// marketplace: fixed offerings, fixed prices, with availability changing
// by region rather than by host.
//
// Every field here is transcribed from /api/v1/instance-types, recorded
// on 2026-08-20 and committed as testdata/instance-types.json.
// TestCatalogTranscribesTheVendorsInstanceTypes checks the rows against
// it, because a hand-copied catalog is only as good as its last reading
// and nothing else in the system can tell that a number is stale.
type SKUSpec struct {
	// Name is the Lambda Labs instance_type_name (e.g. "gpu_1x_a10").
	Name string
	// DisplayName is Lambda's own `description`, verbatim. Copied rather
	// than composed so it can be checked against the recorded response.
	DisplayName string
	// VRAMGb is GPU memory per card, read out of `gpu_description`.
	// Lambda publishes the card's memory nowhere else, which is why this
	// catalog exists at all.
	VRAMGb int
	// GPUCount is the number of GPUs on the instance. Lambda is the one
	// provider whose rows describe a whole instance rather than a card,
	// so a request for eight cards is a request for a row that has eight.
	GPUCount int
	// PriceUSDPerHour is the on-demand price at cataloging.
	// Authoritative price comes back on each /instance-types call.
	PriceUSDPerHour float64
	// Architecture is the host's CPU architecture, normalized. Lambda is the
	// only catalog that states it, because it is the only one selling a
	// shape whose architecture follows from the shape: a GH200 is a Grace
	// Hopper superchip and therefore arm64, and every other Lambda shape is
	// x86_64. An x86 engine image does not start on the former (#390).
	Architecture string
	// Family maps this SKU onto the cross-provider fabric catalog.
	// Lambda names the form factor in the instance type itself
	// ("gpu_1x_a100_sxm4" vs "gpu_1x_h100_pcie"), so the declared tier
	// reads straight off the SKU the same way RunPod's does. Lambda
	// exposes no fabric measurement, so DECLARED is the only tier here.
	Family fabric.Family
}

// skus is the catalog the resolver iterates. Grouped by card count and
// ordered by price within each group, which is how an operator reads it;
// the resolver sorts by price itself and does not depend on the order.
//
// The multi-GPU rows are what make a whole-node request resolvable here.
// Every row used to be one card, so the shared resolver excluded all of
// them before the adapter called Lambda, and a request for eight cards
// got nothing while RunPod and Vast both answered (#380).
//
// Two shapes are deliberately left out, and both are omissions of
// evidence rather than of interest:
//
//   - gpu_8x_v100 and gpu_8x_v100_n. Lambda calls both "Tesla V100 (16
//     GB)" and names no form factor, in the token or in the description.
//     The SXM2 part carries NVLink and the PCIe part carries nothing, so
//     either mapping asserts an interconnect nobody vouched for, in one
//     direction or the other.
//   - gpu_1x_rtx6000. "RTX 6000 (24 GB)" is the Turing Quadro, not the
//     48 GB RTX 6000 Ada that FamilyRTX6000Ada names.
//
// Both remain rentable through an operator-supplied --gpu-sku, which is
// what an uncatalogued shape has always meant here.
var skus = []SKUSpec{
	// One card.
	{Name: "gpu_1x_a6000", DisplayName: "1x A6000 (48 GB)", VRAMGb: 48, GPUCount: 1, PriceUSDPerHour: 1.09, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyA6000},
	{Name: "gpu_1x_a10", DisplayName: "1x A10 (24 GB PCIe)", VRAMGb: 24, GPUCount: 1, PriceUSDPerHour: 1.29, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyA10},
	// Both A100 singles are 40 GB. The 80 GB SXM4 part is sold only as
	// the eight-card shape below, and reading this row as 80 promised
	// 85.9 GB per card to the pre-rent budget check on a card holding
	// 42.9.
	{Name: "gpu_1x_a100", DisplayName: "1x A100 (40 GB PCIe)", VRAMGb: 40, GPUCount: 1, PriceUSDPerHour: 1.99, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyA100PCIe},
	{Name: "gpu_1x_a100_sxm4", DisplayName: "1x A100 (40 GB SXM4)", VRAMGb: 40, GPUCount: 1, PriceUSDPerHour: 1.99, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyA100SXM},
	// GH200 is arm64 and everything else here is x86_64, which nothing in
	// the deploy path checks. See NOTES.md.
	{Name: "gpu_1x_gh200", DisplayName: "1x GH200 (96 GB)", VRAMGb: 96, GPUCount: 1, PriceUSDPerHour: 2.29, Architecture: provisioners.ArchARM64, Family: fabric.FamilyGH200},
	{Name: "gpu_1x_h100_pcie", DisplayName: "1x H100 (80 GB PCIe)", VRAMGb: 80, GPUCount: 1, PriceUSDPerHour: 3.29, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyH100PCIe},
	{Name: "gpu_1x_h100_sxm5", DisplayName: "1x H100 (80 GB SXM5)", VRAMGb: 80, GPUCount: 1, PriceUSDPerHour: 4.29, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyH100SXM},
	{Name: "gpu_1x_b200_sxm6", DisplayName: "1x B200 (180 GB SXM6)", VRAMGb: 180, GPUCount: 1, PriceUSDPerHour: 6.99, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyB200},

	// Two cards.
	{Name: "gpu_2x_a6000", DisplayName: "2x A6000 (48 GB)", VRAMGb: 48, GPUCount: 2, PriceUSDPerHour: 2.18, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyA6000},
	{Name: "gpu_2x_a100", DisplayName: "2x A100 (40 GB PCIe)", VRAMGb: 40, GPUCount: 2, PriceUSDPerHour: 3.98, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyA100PCIe},
	{Name: "gpu_2x_h100_sxm5", DisplayName: "2x H100 (80 GB SXM5)", VRAMGb: 80, GPUCount: 2, PriceUSDPerHour: 8.38, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyH100SXM},
	{Name: "gpu_2x_b200_sxm6", DisplayName: "2x B200 (180 GB SXM6)", VRAMGb: 180, GPUCount: 2, PriceUSDPerHour: 13.78, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyB200},

	// Four cards.
	{Name: "gpu_4x_a6000", DisplayName: "4x A6000 (48 GB)", VRAMGb: 48, GPUCount: 4, PriceUSDPerHour: 4.36, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyA6000},
	{Name: "gpu_4x_a100", DisplayName: "4x A100 (40 GB PCIe)", VRAMGb: 40, GPUCount: 4, PriceUSDPerHour: 7.96, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyA100PCIe},
	{Name: "gpu_4x_h100_sxm5", DisplayName: "4x H100 (80 GB SXM5)", VRAMGb: 80, GPUCount: 4, PriceUSDPerHour: 16.36, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyH100SXM},

	// Eight cards. These are the shapes a frontier sparse model needs, and
	// the SXM rows carry a board-integrated fabric, so they resolve
	// INTRA_NODE from the declared tier alone.
	{Name: "gpu_8x_a100", DisplayName: "8x A100 (40 GB SXM4)", VRAMGb: 40, GPUCount: 8, PriceUSDPerHour: 15.92, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyA100SXM},
	{Name: "gpu_8x_a100_80gb_sxm4", DisplayName: "8x A100 (80 GB SXM4)", VRAMGb: 80, GPUCount: 8, PriceUSDPerHour: 22.32, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyA100SXM},
	{Name: "gpu_8x_h100_sxm5", DisplayName: "8x H100 (80 GB SXM5)", VRAMGb: 80, GPUCount: 8, PriceUSDPerHour: 31.92, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyH100SXM},
	{Name: "gpu_8x_b200_sxm6", DisplayName: "8x B200 (180 GB SXM6)", VRAMGb: 180, GPUCount: 8, PriceUSDPerHour: 53.52, Architecture: provisioners.ArchAMD64, Family: fabric.FamilyB200},
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
			Architecture:    sku.Architecture,
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
