package provisioners_test

import (
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// The bands have to line up with classDefaults, which states the same
// boundaries as VRAM floors. Three adapters used to carry their own copy of
// this switch, so a drift here was a drift in three places at once.
func TestClassifyByVRAMBands(t *testing.T) {
	cases := []struct {
		vramGb int
		want   string
	}{
		{24, provisioners.GPUClassSmall},
		{32, provisioners.GPUClassSmall},
		{39, provisioners.GPUClassSmall},
		{40, provisioners.GPUClassMedium},
		{48, provisioners.GPUClassMedium},
		{79, provisioners.GPUClassMedium},
		{80, provisioners.GPUClassLarge},
		{94, provisioners.GPUClassLarge},
		{95, provisioners.GPUClassLarge},
		{96, provisioners.GPUClassXLarge},
		{192, provisioners.GPUClassXLarge},
	}

	for _, c := range cases {
		if got := provisioners.ClassifyByVRAM(c.vramGb); got != c.want {
			t.Errorf("ClassifyByVRAM(%d) = %q, want %q", c.vramGb, got, c.want)
		}
	}
}

// An uncatalogued card carries no VRAM figure, and the honest answer there is
// no opinion rather than the smallest class. Adapters rely on the empty string
// to leave an operator-supplied --gpu-sku unclassified.
func TestClassifyByVRAMHasNoOpinionWithoutAFigure(t *testing.T) {
	for _, vramGb := range []int{0, -1} {
		if got := provisioners.ClassifyByVRAM(vramGb); got != "" {
			t.Errorf("ClassifyByVRAM(%d) = %q, want empty", vramGb, got)
		}
	}
}
