package skucatalog

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

func archRows() []Entry {
	return []Entry{
		{Token: "gpu_1x_gh200", VRAMGb: 96, GPUCount: 1, PriceUSDPerHour: 2.29, Architecture: "arm64", Family: fabric.FamilyGH200},
		{Token: "gpu_1x_h100_pcie", VRAMGb: 80, GPUCount: 1, PriceUSDPerHour: 3.29, Architecture: "amd64", Family: fabric.FamilyH100PCIe},
		// A catalog that states nothing about its hosts, which is every
		// catalog but Lambda's.
		{Token: "H100_SXM", VRAMGb: 80, GPUCount: 0, PriceUSDPerHour: 2.00, Family: fabric.FamilyH100SXM},
	}
}

// An x86 engine image will not start on an arm64 host, and nothing else in
// the deploy path notices before the container fails on a machine that is
// already billing. Lambda's GH200 is arm64 and the cheapest shape it sells
// clearing 80 GB, so a class=large request resolved it first (#390).
func TestMatchExcludesAnArchitectureTheRequestRefuses(t *testing.T) {
	got := Match(archRows(), &provisionerv1.ResourceRequirements{MinVramGb: 80, Architecture: "amd64"}, FabricDeclared)

	for _, tok := range got {
		if tok == "gpu_1x_gh200" {
			t.Errorf("an arm64 shape satisfied an amd64 request: %v", got)
		}
	}
	if len(got) == 0 {
		t.Fatal("the amd64 rows went missing too")
	}
}

// Unstated means unconstrained, so every request written before the field
// existed resolves what it always did.
func TestMatchIgnoresArchitectureWhenTheRequestStatesNone(t *testing.T) {
	got := Match(archRows(), &provisionerv1.ResourceRequirements{MinVramGb: 80}, FabricDeclared)

	found := false
	for _, tok := range got {
		if tok == "gpu_1x_gh200" {
			found = true
		}
	}
	if !found {
		t.Errorf("gh200 dropped from an unconstrained request: %v", got)
	}
}

// A row whose architecture nobody recorded is kept. Unlike a fabric, this is
// not a capability whose absence costs the whole bill: the failure is a
// container that will not start, found in minutes. Excluding every silent
// row would empty the Vast and RunPod catalogs the moment an operator
// stated an architecture at all.
func TestMatchKeepsRowsThatStateNoArchitecture(t *testing.T) {
	got := Match(archRows(), &provisionerv1.ResourceRequirements{MinVramGb: 80, Architecture: "arm64"}, FabricDeclared)

	var kept []string
	for _, tok := range got {
		kept = append(kept, tok)
	}
	if len(kept) != 2 {
		t.Fatalf("got %v, want the arm64 row and the row that states nothing", kept)
	}
	for _, tok := range kept {
		if tok == "gpu_1x_h100_pcie" {
			t.Errorf("an amd64 row satisfied an arm64 request: %v", kept)
		}
	}
}
