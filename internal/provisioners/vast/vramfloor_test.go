package vast

import (
	"slices"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/skucatalog"
)

// blackwell is the top of the catalog: the parts a frontier run needs and the
// ones a single low floor cannot reach.
var blackwell = []string{"B200", "B300"}

func resolvedAt(vramGb int32) []string {
	return MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: vramGb, GpuCount: 8})
}

// A floor is a floor, and the resolver caps the answer at the cheapest few
// SKUs above it (skucatalog.MaxResults). Both are right on their own and
// together they make a low floor a narrow window: everything above the cap's
// price cut is invisible however much of it the market holds.
//
// This is pinned rather than assumed because a tool was configured against
// the wrong reading of it. hack/capacity-sample.sh sampled one floor of 80
// and recorded six days of eight-card observations with no Blackwell in them,
// while 8x B200 was live on Vast at $47/hr, and that log is what this epic's
// "blocked on capacity" judgements are made from.
func TestALowVRAMFloorCannotReachTheTopOfTheCatalog(t *testing.T) {
	got := resolvedAt(80)
	if len(got) != skucatalog.MaxResults {
		t.Fatalf("a floor of 80 resolved %d SKUs, want the cap of %d; if the catalog changed, re-derive what a sampler has to ask for",
			len(got), skucatalog.MaxResults)
	}
	for _, top := range blackwell {
		if slices.Contains(got, top) {
			t.Errorf("a floor of 80 now reaches %s; the cap or the catalog moved and hack/capacity-sample.sh's floors want re-deriving", top)
		}
	}
}

// And the floor that does reach it, which is what the sampler now asks for.
// If this fails, the sampler is blind again and nobody will notice from its
// output, because absence and unavailability look identical in that log.
func TestAHigherFloorReachesTheBlackwellTier(t *testing.T) {
	got := resolvedAt(140)
	for _, top := range blackwell {
		if !slices.Contains(got, top) {
			t.Errorf("a floor of 140 does not resolve %s: %v", top, got)
		}
	}
}

// The two floors have to overlap nowhere and cover everything, or a sampler
// asking both still has a gap. H100_NVL at 94 GB is the part that would fall
// between a floor of 80 and a floor of 100.
func TestTheTwoSampledFloorsLeaveNoGap(t *testing.T) {
	low, high := resolvedAt(80), resolvedAt(140)
	union := append(slices.Clone(low), high...)

	for _, want := range []string{"A100_SXM4", "H100_SXM", "H100_NVL", "H200", "B200", "B300"} {
		if !slices.Contains(union, want) {
			t.Errorf("%s is reachable by neither sampled floor, so the log cannot show it: low=%v high=%v",
				want, low, high)
		}
	}
}
