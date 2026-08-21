package runpod

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// RunPod publishes the same card as "NVIDIA B300 SXM6 AC" at 288 GB. The
// catalog stopped at B200, so a frontier request resolved nothing here
// either (#354).
func TestMatchSKUsOffersB300ForAFrontierRequest(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 200, GpuCount: 8})

	found := false
	for _, id := range got {
		if id == "NVIDIA B300 SXM6 AC" {
			found = true
		}
	}
	if !found {
		t.Errorf("MatchSKUs = %v, want the B300 type among them", got)
	}
}
