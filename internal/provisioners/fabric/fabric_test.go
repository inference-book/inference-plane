package fabric

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

const (
	scopeUnspec = provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED
	scopeNone   = provisionerv1.FabricScope_FABRIC_SCOPE_NONE
	scopeIntra  = provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE
	scopeInter  = provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE

	srcDeclared = provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED
	srcMeasured = provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED
	srcUnknown  = provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN
)

func TestResolveDeclaredTier(t *testing.T) {
	tests := []struct {
		name       string
		family     Family
		wantScope  provisionerv1.FabricScope
		wantSource provisionerv1.FabricSource
	}{
		{"SXM is always NVLinked", FamilyA100SXM, scopeIntra, srcDeclared},
		{"NVSwitch part", FamilyH100SXM, scopeIntra, srcDeclared},
		{"NVL is a factory-bridged pair", FamilyH100NVL, scopeIntra, srcDeclared},
		{"non-NVIDIA fabric still resolves", FamilyMI300X, scopeIntra, srcDeclared},
		{"card with no fabric at all", FamilyL40S, scopeNone, srcDeclared},
		{"consumer card", FamilyRTX4090, scopeNone, srcDeclared},

		// The row the design doc calls out as the one to pressure-test.
		{"bridge-capable PCIe part is unknown, not none", FamilyA100PCIe, scopeUnspec, srcUnknown},
		{"bridge-capable workstation part is unknown", FamilyA6000, scopeUnspec, srcUnknown},

		{"unrecognized family never guesses", Family("gpu-from-mars"), scopeUnspec, srcUnknown},
		{"empty family never guesses", Family(""), scopeUnspec, srcUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(Observation{Family: tt.family})
			if got.Scope != tt.wantScope || got.Source != tt.wantSource {
				t.Errorf("Resolve(%q) = scope %v / source %v, want %v / %v",
					tt.family, got.Scope, got.Source, tt.wantScope, tt.wantSource)
			}
		})
	}
}

// A zero reading is only "no fabric" when the catalog agrees the card never
// had one. Vast under-reported roughly a quarter of SXM machines in the
// 2026-08-09 probe, so a zero on a board that is physically always NVLinked
// means "not measured" and has to fail closed.
func TestResolveZeroReadingIsNotAlwaysNone(t *testing.T) {
	tests := []struct {
		name       string
		family     Family
		wantScope  provisionerv1.FabricScope
		wantSource provisionerv1.FabricSource
	}{
		{"zero on a card with no fabric is a fact", FamilyL40S, scopeNone, srcMeasured},
		{"zero on an always-NVLinked board means not measured", FamilyA100SXM, scopeUnspec, srcUnknown},
		{"zero on a bridge-capable card means not measured", FamilyA100PCIe, scopeUnspec, srcUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(Observation{Family: tt.family, HasMeasurement: true, MeasuredGbps: 0})
			if got.Scope != tt.wantScope || got.Source != tt.wantSource {
				t.Errorf("zero reading on %q = scope %v / source %v, want %v / %v",
					tt.family, got.Scope, got.Source, tt.wantScope, tt.wantSource)
			}
		})
	}
}

// A positive provider reading outranks the catalog. The live case: 3 of 24
// Vast "A100 PCIE" offers reported 275-300 GB/s, meaning a bridge is
// installed on a card whose name says PCIe. A name-only catalog excludes it.
func TestResolveMeasuredOutranksDeclared(t *testing.T) {
	got := Resolve(Observation{Family: FamilyA100PCIe, HasMeasurement: true, MeasuredGbps: 2400})
	if got.Scope != scopeIntra {
		t.Errorf("bridged A100 PCIe scope = %v, want %v", got.Scope, scopeIntra)
	}
	if got.Source != srcMeasured {
		t.Errorf("bridged A100 PCIe source = %v, want %v", got.Source, srcMeasured)
	}
	if got.Gbps != 2400 {
		t.Errorf("bridged A100 PCIe gbps = %d, want the measured 2400 not the catalog peak", got.Gbps)
	}
}

func TestResolveNeverReportsInterNode(t *testing.T) {
	for family := range catalog {
		for _, obs := range []Observation{
			{Family: family},
			{Family: family, HasMeasurement: true, MeasuredGbps: 99999},
			{Family: family, HasMeasurement: true, MeasuredGbps: 0},
		} {
			if got := Resolve(obs); got.Scope == scopeInter {
				t.Fatalf("Resolve(%+v) reported INTER_NODE; no provider exposes a cross-node "+
					"fabric on a single rented instance (that arrives with issue 212)", obs)
			}
		}
	}
}

func TestSatisfies(t *testing.T) {
	intraDeclared := Resolve(Observation{Family: FamilyA100SXM})
	noneDeclared := Resolve(Observation{Family: FamilyL40S})
	unknown := Resolve(Observation{Family: FamilyA100PCIe})

	tests := []struct {
		name      string
		got       Result
		wantScope provisionerv1.FabricScope
		minGbps   int32
		want      bool
	}{
		{"no requirement admits everything", intraDeclared, scopeUnspec, 0, true},
		{"no requirement admits unknown too", unknown, scopeUnspec, 0, true},

		// Rule 1, the whole point of the field.
		{"unknown never satisfies a set requirement", unknown, scopeIntra, 0, false},
		{"unknown never satisfies even at zero bandwidth floor", unknown, scopeNone, 0, false},

		{"matching scope passes", intraDeclared, scopeIntra, 0, true},
		{"mismatched scope fails", noneDeclared, scopeIntra, 0, false},
		{"intra does not satisfy inter", intraDeclared, scopeInter, 0, false},

		{"bandwidth floor met", intraDeclared, scopeIntra, 2400, true},
		{"bandwidth floor exactly met", intraDeclared, scopeIntra, 4800, true},
		{"bandwidth floor unmet", intraDeclared, scopeIntra, 9600, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Satisfies(tt.got, tt.wantScope, tt.minGbps); got != tt.want {
				t.Errorf("Satisfies(%+v, %v, %d) = %v, want %v",
					tt.got, tt.wantScope, tt.minGbps, got, tt.want)
			}
		})
	}
}

// Every catalog entry must be internally consistent: a family that can carry
// a fabric needs a bandwidth figure, and one that cannot must not have one.
// Guards against a copy-paste row silently claiming 0 Gbps of NVLink.
func TestCatalogEntriesAreConsistent(t *testing.T) {
	for family, spec := range catalog {
		switch spec.Capability {
		case CapabilityNone:
			if spec.PeakGbps != 0 || spec.Technology != "" {
				t.Errorf("%q has no fabric but declares tech %q at %d Gbps",
					family, spec.Technology, spec.PeakGbps)
			}
		default:
			if spec.PeakGbps <= 0 {
				t.Errorf("%q can carry a fabric but declares %d Gbps", family, spec.PeakGbps)
			}
			if spec.Technology == "" {
				t.Errorf("%q can carry a fabric but names no technology", family)
			}
		}
	}
}

// CouldSatisfy must be more permissive than Satisfies for exactly one class
// of candidate: the bridge-capable card a provider reading could rescue.
// Getting this backwards silently discards the machines the measured tier
// exists to find.
func TestCouldSatisfyIsPermissiveWhereSatisfiesIsNot(t *testing.T) {
	const bridgeCapable = FamilyA100PCIe

	if !CouldSatisfy(bridgeCapable, scopeIntra) {
		t.Error("bridge-capable card excluded from an intra-node SEARCH; a reading could still rescue it")
	}
	if Satisfies(Resolve(Observation{Family: bridgeCapable}), scopeIntra, 0) {
		t.Error("bridge-capable card ADMITTED without a reading; unknown must fail closed")
	}
}

func TestCouldSatisfy(t *testing.T) {
	tests := []struct {
		name   string
		family Family
		want   provisionerv1.FabricScope
		ok     bool
	}{
		{"no requirement searches everything", FamilyRTX4090, scopeUnspec, true},
		{"always-linked card for intra", FamilyA100SXM, scopeIntra, true},
		{"bridge-capable card for intra", FamilyA6000, scopeIntra, true},
		{"card with no link is impossible for intra", FamilyL40S, scopeIntra, false},
		{"card with no link is what NONE wants", FamilyL40S, scopeNone, true},
		{"linked card does not match NONE", FamilyA100SXM, scopeNone, false},
		{"inter-node is not derivable from one card", FamilyH100SXM, scopeInter, false},
		{"unknown family is never worth searching", Family("mystery"), scopeIntra, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CouldSatisfy(tt.family, tt.want); got != tt.ok {
				t.Errorf("CouldSatisfy(%q, %v) = %v, want %v", tt.family, tt.want, got, tt.ok)
			}
		})
	}
}

func TestGbpsFromGBps(t *testing.T) {
	tests := []struct {
		gbytes float64
		want   int32
	}{
		{300, 2400}, // Vast's A100 SXM4 reading
		{900, 7200}, // Vast's H100 SXM reading
		{56, 448},   // RTX A6000 bridge
		{0, 0},
	}
	for _, tt := range tests {
		if got := GbpsFromGBps(tt.gbytes); got != tt.want {
			t.Errorf("GbpsFromGBps(%v) = %d, want %d", tt.gbytes, got, tt.want)
		}
	}
}
