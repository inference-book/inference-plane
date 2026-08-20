package lambdalabs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// A whole-node request has to resolve here at all. Every catalog row was
// GPUCount 1, so the shared resolver excluded all of them before the
// adapter ever called Lambda, and `iplane capacity --gpu-count 8` reported
// nothing while RunPod and Vast both answered (#380).
func TestMatchSKUsResolvesWholeNodeShapes(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{
		GpuCount:    8,
		MinVramGb:   80,
		FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	})

	want := []string{"gpu_8x_a100_80gb_sxm4", "gpu_8x_h100_sxm5", "gpu_8x_b200_sxm6"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("MatchSKUs = %v, want %v (cheapest first)", got, want)
	}
}

// Lambda reports no interconnect measurement, so the catalog is the last
// word and a bridge-capable card resolves to UNKNOWN rather than to a
// link nobody vouched for. The PCIe A100 and the A6000 are exactly those
// cards, and renting one to find out costs money.
func TestMatchSKUsDropsBridgeCapableShapesWhenFabricIsRequired(t *testing.T) {
	bridged := []string{"gpu_4x_a100", "gpu_4x_a6000"}

	withFabric := MatchSKUs(&provisionerv1.ResourceRequirements{
		GpuCount: 4, MinVramGb: 40,
		FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	})
	for _, name := range bridged {
		if contains(withFabric, name) {
			t.Errorf("%q satisfied an intra-node request on a declared-only provider: %v", name, withFabric)
		}
	}

	// And they are not simply missing from the catalog, which would pass
	// the check above for the wrong reason.
	withoutFabric := MatchSKUs(&provisionerv1.ResourceRequirements{GpuCount: 4, MinVramGb: 40})
	for _, name := range bridged {
		if !contains(withoutFabric, name) {
			t.Errorf("%q is absent even with no fabric asked for: %v", name, withoutFabric)
		}
	}
}

// A Lambda row is a whole instance, so a one-card request that resolved an
// eight-card shape would rent, and bill for, seven cards nobody asked for.
// The resolver keeps such a row eligible, since it does satisfy the count,
// and price ordering is what keeps it off the front of the list.
func TestMatchSKUsKeepsAOneCardRequestOnAOneCardShape(t *testing.T) {
	for _, vram := range []int32{24, 40, 48, 80, 180} {
		got := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: vram})
		if len(got) == 0 {
			t.Errorf("min_vram_gb %d matched nothing", vram)
			continue
		}
		spec := LookupSKU(got[0])
		if spec == nil {
			t.Errorf("min_vram_gb %d resolved %q, which is not in the catalog", vram, got[0])
			continue
		}
		if spec.GPUCount != 1 {
			t.Errorf("min_vram_gb %d resolved %q first, a %d-card box, for a one-card request",
				vram, got[0], spec.GPUCount)
		}
	}
}

var vramInDescription = regexp.MustCompile(`\((\d+) GB`)

// loadInstanceTypes reads the recorded /instance-types response.
//
// Recorded live on 2026-08-20 with the adapter's own credentials, trimmed
// to the fields the catalog transcribes. Decoded through the production
// types rather than a test-local struct, so a fixture the adapter could
// not parse fails here rather than passing quietly.
func loadInstanceTypes(t *testing.T) map[string]instanceTypesEntry {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "instance-types.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var out map[string]instanceTypesEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return out
}

// The catalog is hand-copied from Lambda's /instance-types, and the field
// that decides a rental is the one nothing else checked. gpu_1x_a100_sxm4
// carried VRAMGb 80 against a card Lambda calls "A100 (40 GB SXM4)", which
// promised 85.9 GB per card to the pre-rent budget check on a card holding
// 42.9, and the display name beside it was wrong in the same direction, so
// the two agreed with each other and with nothing else.
//
// Against the vendor's recorded answer, then, rather than against the row's
// own prose. VRAM is read out of gpu_description because Lambda publishes
// the card's memory nowhere else, which is the reason this catalog exists.
func TestCatalogTranscribesTheVendorsInstanceTypes(t *testing.T) {
	fixture := loadInstanceTypes(t)

	for _, sku := range skus {
		entry, ok := fixture[sku.Name]
		if !ok {
			t.Errorf("%s is in the catalog and not in Lambda's instance types", sku.Name)
			continue
		}
		it := entry.InstanceType

		if sku.DisplayName != it.Description {
			t.Errorf("%s: DisplayName %q, Lambda says %q", sku.Name, sku.DisplayName, it.Description)
		}
		if sku.GPUCount != it.Specs.GPUs {
			t.Errorf("%s: GPUCount %d, Lambda says %d", sku.Name, sku.GPUCount, it.Specs.GPUs)
		}
		if want := float64(it.PriceCentsPerHour) / 100; sku.PriceUSDPerHour != want {
			t.Errorf("%s: price %.2f, Lambda says %.2f", sku.Name, sku.PriceUSDPerHour, want)
		}

		m := vramInDescription.FindStringSubmatch(it.GPUDescription)
		if m == nil {
			t.Errorf("%s: Lambda's %q names no card memory, so the row cannot be checked",
				sku.Name, it.GPUDescription)
			continue
		}
		if gb, _ := strconv.Atoi(m[1]); sku.VRAMGb != gb {
			t.Errorf("%s: VRAMGb %d, Lambda says %q", sku.Name, sku.VRAMGb, it.GPUDescription)
		}
	}
}

func contains(haystack []string, want string) bool {
	for _, s := range haystack {
		if s == want {
			return true
		}
	}
	return false
}
