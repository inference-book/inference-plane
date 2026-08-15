package skucatalog

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

// Reclaimability is settled per candidate, never per catalog row, and the
// catalog stage must stay out of it.
//
// The first attempt put a plain bool on Entry. A bool has no way to say "this
// catalog cannot know", so "unknown" and "not reclaimable" collapsed into
// false and asking for reclaimable capacity matched nothing on any provider.
// Same trap as disk (#281) and system RAM (#283), third time.
func TestReclaimPolicyDoesNotFilterTheCatalog(t *testing.T) {
	catalog := []Entry{
		{Token: "a100", VRAMGb: 80, PriceUSDPerHour: 1.79, Family: fabric.FamilyA100SXM},
	}

	for _, policy := range []provisionerv1.ReclaimPolicy{
		provisionerv1.ReclaimPolicy_RECLAIM_POLICY_UNSPECIFIED,
		provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED,
		provisionerv1.ReclaimPolicy_RECLAIM_POLICY_NEVER,
	} {
		got := Match(catalog, &provisionerv1.ResourceRequirements{ReclaimPolicy: policy}, FabricDeclared)
		if len(got) != 1 {
			t.Errorf("policy %v dropped the row; the catalog cannot know the tier, so it must not filter on it", policy)
		}
	}
}
