package provisioners_test

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

func archCandidates() []*provisionerv1.Candidate {
	return []*provisionerv1.Candidate{
		{Sku: "gpu_1x_gh200", Architecture: provisioners.ArchARM64, PriceUsdPerHour: 2.29},
		{Sku: "A100_SXM4", Architecture: provisioners.ArchAMD64, PriceUsdPerHour: 10.40},
		// RunPod reports no architecture at all.
		{Sku: "NVIDIA H100 80GB HBM3", PriceUsdPerHour: 21.52},
	}
}

// A listing that offers a shape the operator's image cannot run on is
// offering a rental that fails at container start on a billing machine.
func TestFilterArchitectureDropsWhatTheRequestRefuses(t *testing.T) {
	got := provisioners.FilterArchitecture(archCandidates(), provisioners.ArchAMD64)

	for _, c := range got {
		if c.GetArchitecture() == provisioners.ArchARM64 {
			t.Errorf("an arm64 candidate survived an amd64 request: %+v", c)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want the amd64 one and the one reporting nothing", len(got))
	}
}

// A candidate whose architecture nobody reported is kept, because RunPod
// reports none at all and dropping silence would empty its listing the
// moment an operator stated an architecture.
func TestFilterArchitectureKeepsCandidatesThatReportNothing(t *testing.T) {
	got := provisioners.FilterArchitecture(archCandidates(), provisioners.ArchARM64)

	var skus []string
	for _, c := range got {
		skus = append(skus, c.GetSku())
	}
	if len(skus) != 2 {
		t.Fatalf("got %v, want the arm64 candidate and the silent one", skus)
	}
}

// Unstated is unconstrained, so a listing nobody narrowed is untouched.
func TestFilterArchitectureIsANoOpWhenUnstated(t *testing.T) {
	if got := provisioners.FilterArchitecture(archCandidates(), ""); len(got) != 3 {
		t.Errorf("got %d candidates, want all 3 left alone", len(got))
	}
}
