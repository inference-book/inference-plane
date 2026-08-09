// Package fabric holds the cross-provider knowledge of what GPU
// interconnect a card is capable of, and the rules for deciding whether a
// candidate satisfies an operator's fabric requirement.
//
// It lives outside the adapters because "does an RTX A6000 support NVLink"
// is a fact about NVIDIA's product line, not about RunPod or Vast. Each
// adapter maps its own SKU vocabulary onto Family, and the answers come back
// vendor-neutral (provisionerv1.FabricScope), so provider-specific naming
// stays in the adapter exactly as the rest of the provisioner seams do.
//
// The design rationale, and the live provider probes the tier rules are
// derived from, are in docs/design/0006-ch10-provider-reality-and-control-channel.md.
package fabric

import (
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Capability is what a GPU model is physically able to do, before any
// particular host is considered. It is the catalog axis: three states, not
// two, because "this card can be bridged but the provider does not say
// whether this one is" is a genuinely different answer from either yes or no.
type Capability int

const (
	// CapabilityNone means the card has no intra-node fabric at all. An
	// RTX 4090 or an L40S: cards that talk over PCIe and nothing else.
	// A zero bandwidth reading on one of these is real information.
	CapabilityNone Capability = iota

	// CapabilityAlways means the fabric is part of the board. SXM and NVL
	// parts are soldered onto an NVSwitch mesh or a bridged pair; there is
	// no configuration in which an SXM4 A100 lacks NVLink.
	CapabilityAlways

	// CapabilityOptional means the card supports a bridge that the host
	// operator may or may not have installed. A100 PCIe, H100 PCIe, RTX
	// A5000/A6000. This is the state that forces FABRIC_SOURCE_UNKNOWN:
	// the SKU name cannot answer the question and most providers do not.
	CapabilityOptional
)

// Family is a vendor-neutral GPU model identifier. Adapters normalize their
// own SKU strings onto these (RunPod's "NVIDIA A100-SXM4-80GB" and Vast's
// "A100 SXM4" are both FamilyA100SXM), so the catalog is written once.
type Family string

// Known GPU families. The list covers what our providers actually offer;
// an unrecognized family resolves to UNKNOWN rather than to a guess.
const (
	FamilyA100SXM    Family = "a100-sxm"
	FamilyA100PCIe   Family = "a100-pcie"
	FamilyH100SXM    Family = "h100-sxm"
	FamilyH100PCIe   Family = "h100-pcie"
	FamilyH100NVL    Family = "h100-nvl"
	FamilyH200SXM    Family = "h200-sxm"
	FamilyH200NVL    Family = "h200-nvl"
	FamilyB200       Family = "b200"
	FamilyB300       Family = "b300"
	FamilyMI300X     Family = "mi300x"
	FamilyA10        Family = "a10"
	FamilyGH200      Family = "gh200"
	FamilyA6000      Family = "rtx-a6000"
	FamilyA5000      Family = "rtx-a5000"
	FamilyL40S       Family = "l40s"
	FamilyL40        Family = "l40"
	FamilyL4         Family = "l4"
	FamilyA40        Family = "a40"
	FamilyRTX6000Ada Family = "rtx-6000-ada"
	FamilyRTX4090    Family = "rtx-4090"
	FamilyRTX5090    Family = "rtx-5090"
	FamilyV100SXM    Family = "v100-sxm"
	FamilyV100PCIe   Family = "v100-pcie"
)

// Spec is the catalog entry for one GPU family.
type Spec struct {
	// Capability is what the silicon can do.
	Capability Capability
	// Technology names the vendor's fabric ("nvlink", "xgmi"). Diagnostic
	// only; it reaches Hardware.fabric_technology and nothing branches on it.
	Technology string
	// PeakGbps is the card's aggregate per-GPU fabric bandwidth in GIGABITS
	// per second when the fabric is present, taken from the vendor's
	// BIDIRECTIONAL per-GPU figure times 8 (an A100 SXM's published 600 GB/s
	// is 4800 here). 0 when the family has no fabric.
	PeakGbps int32
}

// catalog is the declared tier: what each family is capable of, independent
// of any host. Bandwidth figures are NVIDIA/AMD published per-GPU
// bidirectional aggregates converted to gigabits.
//
// This table answers "could this card have a fabric", never "does this
// particular rented machine have one". Only a provider reading answers that,
// which is why Resolve takes both.
//
// Treat cross-source bandwidth comparisons as coarse. Vast's own readings are
// not consistent about direction: probing live on 2026-08-09 it reported 300
// for A100 SXM4 (unidirectional, against a 600 GB/s published figure) but 900
// for H100 SXM (bidirectional). So min_fabric_gbps is dependable for
// separating fabric tiers and not for fine thresholds near a card's rating.
var catalog = map[Family]Spec{
	// Board-integrated fabric. No configuration lacks it.
	FamilyA100SXM: {CapabilityAlways, "nvlink", 4800},
	FamilyH100SXM: {CapabilityAlways, "nvswitch", 7200},
	FamilyH200SXM: {CapabilityAlways, "nvswitch", 7200},
	FamilyB200:    {CapabilityAlways, "nvswitch", 14400},
	FamilyB300:    {CapabilityAlways, "nvswitch", 14400},
	FamilyV100SXM: {CapabilityAlways, "nvlink", 2400},
	// MI300X's fabric is xGMI, not NVLink. It is INTRA_NODE all the same,
	// and it is the case a vendor-named enum could never have expressed.
	FamilyMI300X: {CapabilityAlways, "xgmi", 8192},

	// GH200 superchips carry NVLink between GPUs. Note the catalog describes
	// the CARD, not the rented shape: Lambda only sells gpu_1x_gh200, where
	// there is no second card to link to. Requiring a fabric on a one-GPU
	// instance is rejected up front (see the service-layer validation) rather
	// than resolved here.
	FamilyGH200: {CapabilityAlways, "nvlink", 7200},

	// NVL parts are bridged pairs from the factory.
	FamilyH100NVL: {CapabilityAlways, "nvlink", 4800},
	FamilyH200NVL: {CapabilityAlways, "nvlink", 4800},

	// Bridge-capable: the card supports NVLink, the host may not have
	// installed it, and the provider does not say. Probing Vast live,
	// 3 of 24 "A100 PCIE" offers reported 275-300 GB/s, so treating these
	// as NONE would wrongly exclude real working machines.
	FamilyA100PCIe: {CapabilityOptional, "nvlink", 4800},
	FamilyH100PCIe: {CapabilityOptional, "nvlink", 4800},
	FamilyA6000:    {CapabilityOptional, "nvlink", 896},
	FamilyA5000:    {CapabilityOptional, "nvlink", 896},

	// No intra-node fabric at all. A zero reading on these is a fact.
	FamilyA10:        {CapabilityNone, "", 0},
	FamilyL40S:       {CapabilityNone, "", 0},
	FamilyL40:        {CapabilityNone, "", 0},
	FamilyL4:         {CapabilityNone, "", 0},
	FamilyA40:        {CapabilityNone, "", 0},
	FamilyRTX6000Ada: {CapabilityNone, "", 0},
	FamilyRTX4090:    {CapabilityNone, "", 0},
	FamilyRTX5090:    {CapabilityNone, "", 0},
	FamilyV100PCIe:   {CapabilityNone, "", 0},
}

// Observation is what an adapter learned about one candidate: which family
// it is, and what the provider reported about its fabric, if anything.
type Observation struct {
	// Family is the normalized GPU model. Empty or unrecognized yields an
	// UNKNOWN result rather than a default.
	Family Family

	// MeasuredGbps is a provider-reported fabric bandwidth in gigabits per
	// second. Only Vast supplies one today (bw_nvlink, converted from GB/s).
	//
	// HasMeasurement, not a zero check, is what marks this as present. Vast
	// reports 0.0 both for "this machine has no NVLink" and for "we never
	// measured it", so the zero has to be interpreted against the catalog
	// rather than trusted, which is what Resolve does.
	MeasuredGbps   int32
	HasMeasurement bool
}

// Result is the resolved fabric of a candidate, ready to be stamped onto
// Hardware or compared against a requirement.
type Result struct {
	Scope      provisionerv1.FabricScope
	Source     provisionerv1.FabricSource
	Gbps       int32
	Technology string
}

// Resolve turns a catalog entry plus an optional provider reading into a
// fabric verdict, applying the three rules docs/design/0006 Part 2 derives
// from live provider data:
//
//  1. An unknown fabric stays UNKNOWN and (via Satisfies) fails closed.
//  2. A zero reading means "no fabric" only when the catalog agrees the card
//     has none. On a card that should have had one it means "not measured",
//     because Vast under-reported roughly a quarter of SXM machines and the
//     payload cannot distinguish the two.
//  3. A real measurement outranks the catalog, since a bridged card named
//     "PCIe" is a machine a name-only catalog would wrongly exclude.
//
// Resolve never reports INTER_NODE: no provider we have exposes a cross-node
// fabric on a single rented instance. That scope arrives with the group
// provisioning capability (issue 212).
func Resolve(obs Observation) Result {
	spec, known := catalog[obs.Family]
	if !known {
		return Result{
			Scope:  provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			Source: provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN,
		}
	}

	if obs.HasMeasurement {
		if obs.MeasuredGbps > 0 {
			// Rule 3: a positive reading is the strongest evidence there is,
			// including on a card the catalog only calls bridge-capable.
			return Result{
				Scope:      provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
				Source:     provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED,
				Gbps:       obs.MeasuredGbps,
				Technology: spec.Technology,
			}
		}
		// Rule 2: zero is only trustworthy when the catalog agrees there was
		// never a link to find.
		if spec.Capability == CapabilityNone {
			return Result{
				Scope:  provisionerv1.FabricScope_FABRIC_SCOPE_NONE,
				Source: provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED,
			}
		}
		return Result{
			Scope:  provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			Source: provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN,
		}
	}

	switch spec.Capability {
	case CapabilityAlways:
		return Result{
			Scope:      provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
			Source:     provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED,
			Gbps:       spec.PeakGbps,
			Technology: spec.Technology,
		}
	case CapabilityNone:
		return Result{
			Scope:  provisionerv1.FabricScope_FABRIC_SCOPE_NONE,
			Source: provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED,
		}
	default: // CapabilityOptional, and no provider reading to settle it.
		return Result{
			Scope:  provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			Source: provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN,
		}
	}
}

// Satisfies reports whether a resolved candidate meets a requirement.
//
// Rule 1 lives here: an UNKNOWN source never satisfies a set requirement, so
// a candidate we cannot vouch for is dropped rather than rented and measured
// afterwards. The operator's override is ResourceRequirements.sku, which
// skips resolution entirely; there is no separate "allow unknown" flag,
// because one would make the guarantee optional and therefore worthless.
//
// A zero wantScope means the operator expressed no fabric preference and
// everything satisfies it, including UNKNOWN. That is the Ch 6-9 default and
// it must stay free.
func Satisfies(got Result, wantScope provisionerv1.FabricScope, minGbps int32) bool {
	if wantScope == provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED {
		return true
	}
	if got.Source == provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN {
		return false
	}
	if got.Scope != wantScope {
		return false
	}
	return minGbps <= 0 || got.Gbps >= minGbps
}
