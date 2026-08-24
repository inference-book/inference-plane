package lambdalabs

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// The trap, pinned. With the A100 SXM4 correctly catalogued at 40 GB, the
// cheapest Lambda shape clearing 80 GB is the GH200, and the GH200 is arm64.
// So a one-card request for a large card resolves an arm64 box first, and an
// x86 engine image will not start on it.
//
// This is not hypothetical arithmetic: it is what `--class large` and
// `--min-vram-gb 80` do on this provider today, which is why #405 exists.
func TestUnconstrainedLargeCardRequestResolvesAnARM64Box(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 80, GpuCount: 1})
	if len(got) == 0 {
		t.Fatal("no SKU clears 80 GB, which contradicts the catalog")
	}
	if got[0] != "gpu_1x_gh200" {
		t.Fatalf("cheapest 80 GB shape = %q, want gpu_1x_gh200; if the catalog changed, this test is the thing to re-derive", got[0])
	}
	if arch := LookupSKU(got[0]).Architecture; arch != provisioners.ArchARM64 {
		t.Fatalf("gpu_1x_gh200 architecture = %q, want arm64", arch)
	}
}

// And the fix. Once the requirements carry the architecture the image needs,
// the resolver never offers the box in the first place. The deploy path
// supplies that from the registry when the operator did not (#405); this is
// the half that was already built and had no way to be told.
func TestStatingTheArchitectureKeepsTheARM64BoxOutOfTheResult(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{
		MinVramGb: 80, GpuCount: 1, Architecture: provisioners.ArchAMD64,
	})
	if len(got) == 0 {
		t.Fatal("no amd64 SKU clears 80 GB, which would make the filter useless")
	}
	for _, sku := range got {
		if e := LookupSKU(sku); e != nil && e.Architecture == provisioners.ArchARM64 {
			t.Errorf("resolver offered %q, which is arm64, to a request that said amd64", sku)
		}
	}
	if got[0] == "gpu_1x_gh200" {
		t.Error("the arm64 box is still first")
	}
}
