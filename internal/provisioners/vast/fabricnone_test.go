package vast

import (
	"context"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// "Do not care" and "must not have one" are different requests, and the
// difference decides whether an experiment is valid. Ch 10's A/B control arm
// needs the second, and before this it could only be spelled as the first.
func TestFabricNonePushesACeiling(t *testing.T) {
	var gotQuery map[string]any
	p, _ := newTestProvider(t, captureQuery(t, &gotQuery))

	if _, err := p.findOffer(context.Background(), "A100_PCIE", 4, 0,
		&provisionerv1.ResourceRequirements{
			FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_NONE,
		}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}

	bw, ok := gotQuery["bw_nvlink"].(map[string]any)
	if !ok {
		t.Fatalf("a no-fabric request sent no bw_nvlink constraint, so a bridged host can still be picked: %v", gotQuery)
	}
	lte, has := bw["lte"].(float64)
	if !has {
		t.Fatalf("bw_nvlink = %v, want an lte ceiling", bw)
	}
	if lte != 0 {
		t.Errorf("bw_nvlink lte = %v, want 0: anything above admits a bridged card", lte)
	}
	// A ceiling must not arrive alongside a floor. Both at once is an
	// unsatisfiable query that would report as "no capacity".
	if _, hasGte := bw["gte"]; hasGte {
		t.Errorf("bw_nvlink carries both a floor and a ceiling: %v", bw)
	}
}

// The two directions must stay distinct. A NONE request that pushed the
// INTRA_NODE floor would return exactly the hosts it is meant to exclude,
// which is the worst possible failure here: the A/B would look clean and
// compare NVLink against NVLink.
func TestFabricNoneAndIntraNodeAreOpposites(t *testing.T) {
	for _, tc := range []struct {
		name     string
		scope    provisionerv1.FabricScope
		wantKey  string
		wrongKey string
	}{
		{"intra-node wants a floor", provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE, "gte", "lte"},
		{"none wants a ceiling", provisionerv1.FabricScope_FABRIC_SCOPE_NONE, "lte", "gte"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery map[string]any
			p, _ := newTestProvider(t, captureQuery(t, &gotQuery))

			if _, err := p.findOffer(context.Background(), "A100_PCIE", 4, 0,
				&provisionerv1.ResourceRequirements{FabricScope: tc.scope}); err != nil {
				t.Fatalf("findOffer: %v", err)
			}
			bw, ok := gotQuery["bw_nvlink"].(map[string]any)
			if !ok {
				t.Fatalf("no bw_nvlink constraint: %v", gotQuery)
			}
			if _, has := bw[tc.wantKey]; !has {
				t.Errorf("bw_nvlink = %v, want a %q bound", bw, tc.wantKey)
			}
			if _, has := bw[tc.wrongKey]; has {
				t.Errorf("bw_nvlink = %v carries the opposite bound %q", bw, tc.wrongKey)
			}
		})
	}
}

// Unset must stay free. Ch 6-9 deploys express no fabric preference and must
// not start paying a filter they never asked for.
func TestUnspecifiedFabricStillSendsNoConstraint(t *testing.T) {
	var gotQuery map[string]any
	p, _ := newTestProvider(t, captureQuery(t, &gotQuery))

	if _, err := p.findOffer(context.Background(), "A100_PCIE", 4, 0,
		&provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}
	if _, present := gotQuery["bw_nvlink"]; present {
		t.Errorf("an unconstrained request sent a fabric filter: %v", gotQuery)
	}
}

// The honest limit of the guarantee, pinned so nobody later reads the filter
// as proof of absence.
//
// Vast reports 0 both for "no link" and for "never measured", and the probe
// that found bridged PCIe hosts also found roughly a quarter of SXM machines
// reporting zero on boards that are physically always NVLinked. So a
// bridge-capable card with a zero reading resolves to UNKNOWN, not to NONE,
// and the Hardware record must keep saying so.
func TestZeroReadingOnABridgeCapableCardStaysUnknown(t *testing.T) {
	hw := &provisionerv1.Hardware{}
	zero := 0.0
	stampFabric(hw, "A100_PCIE", &zero)

	if hw.GetFabricSource() != provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN {
		t.Errorf("fabric_source = %v, want UNKNOWN: a zero reading on a bridge-capable card is not evidence of absence",
			hw.GetFabricSource())
	}
	if hw.GetFabricScope() == provisionerv1.FabricScope_FABRIC_SCOPE_NONE {
		t.Error("fabric_scope = NONE, which claims a certainty the reading does not support")
	}
}
