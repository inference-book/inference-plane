package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Vast sells physically different cards under one gpu_name. iplane needs its
// own token per variant, but the marketplace only knows the shared one, so the
// token must be translated at the wire boundary. Sending "A100 SXM4 40GB"
// would filter on a gpu_name that does not exist and return an empty offer
// list, which is indistinguishable from "no capacity right now".
func TestVariantSKUSendsTheSharedWireName(t *testing.T) {
	var gotQuery map[string]any
	p, _ := newTestProvider(t, captureQuery(t, &gotQuery))

	if _, err := p.findOffer(context.Background(), "A100_SXM4_40GB", 1, 0,
		&provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}

	name := gotQuery["gpu_name"].(map[string]any)["eq"].(string)
	if name != "A100 SXM4" {
		t.Errorf("gpu_name = %q, want %q (the marketplace has no 40GB-suffixed name)", name, "A100 SXM4")
	}
}

// The variant is only distinguishable by its memory, so the search has to
// bound it on both sides. A floor alone lets the 80 GB part through, since 80
// clears a 40 GB floor.
func TestVariantSKUBoundsVRAMOnBothSides(t *testing.T) {
	var gotQuery map[string]any
	p, _ := newTestProvider(t, captureQuery(t, &gotQuery))

	if _, err := p.findOffer(context.Background(), "A100_SXM4_40GB", 1, 0,
		&provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}

	band, ok := gotQuery["gpu_ram"].(map[string]any)
	if !ok {
		t.Fatalf("no gpu_ram constraint: %v", gotQuery)
	}
	gte, hasGte := band["gte"].(float64)
	lte, hasLte := band["lte"].(float64)
	if !hasGte || !hasLte {
		t.Fatalf("gpu_ram = %v, want both gte and lte", band)
	}
	// The band must admit a real 40 GB card and exclude a real 80 GB one.
	// Observed readings: 40 GB parts report about 41000, 80 GB about 82000.
	const real40, real80 = 41000, 82000
	if real40 < gte || real40 > lte {
		t.Errorf("band [%v,%v] excludes a real 40GB card reporting %d", gte, lte, real40)
	}
	if real80 >= gte && real80 <= lte {
		t.Errorf("band [%v,%v] admits an 80GB card reporting %d, which is the collision this exists to prevent", gte, lte, real80)
	}
}

// #243's guarantee. Asking for the 80 GB SKU must still never hand back the
// 40 GB card, which is the bug the floor was introduced to fix. Adding the
// variant rows must not reopen it.
func TestEightyGBSKUStillExcludesTheFortyGBCard(t *testing.T) {
	var gotQuery map[string]any
	p, _ := newTestProvider(t, captureQuery(t, &gotQuery))

	if _, err := p.findOffer(context.Background(), "A100_SXM4", 1, 0,
		&provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}

	gte := gotQuery["gpu_ram"].(map[string]any)["gte"].(float64)
	if gte <= 41000 {
		t.Errorf("gpu_ram floor = %v, low enough to admit a 40GB card: #243 regressed", gte)
	}
}

// A SKU that is the only row for its gpu_name has nothing to disambiguate, and
// capping it would reject a host reporting slightly more memory than the
// catalog claims.
func TestNonVariantSKUSendsNoCeiling(t *testing.T) {
	var gotQuery map[string]any
	p, _ := newTestProvider(t, captureQuery(t, &gotQuery))

	if _, err := p.findOffer(context.Background(), "RTX_4090", 1, 0,
		&provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}

	if band, ok := gotQuery["gpu_ram"].(map[string]any); ok {
		if _, has := band["lte"]; has {
			t.Errorf("unbounded SKU sent a ceiling: %v", band)
		}
	}
}

// Both rows must be independently addressable, and must not shadow each other
// in the catalog lookup.
func TestBothA100RowsResolveDistinctly(t *testing.T) {
	for _, tc := range []struct {
		sku      string
		wantVRAM int
		wantWire string
	}{
		{"A100_SXM4_40GB", 40, "A100_SXM4"},
		{"A100_SXM4", 80, ""},
		{"A100_PCIE_40GB", 40, "A100_PCIE"},
		{"A100_PCIE", 80, ""},
	} {
		spec := LookupSKU(tc.sku)
		if spec == nil {
			t.Errorf("%s: not in the catalog", tc.sku)
			continue
		}
		if spec.VRAMGb != tc.wantVRAM {
			t.Errorf("%s: VRAMGb = %d, want %d", tc.sku, spec.VRAMGb, tc.wantVRAM)
		}
		if spec.WireName != tc.wantWire {
			t.Errorf("%s: WireName = %q, want %q", tc.sku, spec.WireName, tc.wantWire)
		}
	}
}

// The resolver should reach the 40 GB tier for a request it genuinely
// satisfies, and must never rank the dearer 80 GB row above its 40 GB
// counterpart.
//
// Asserted on whichever A100 variants survive MaxSKUsPerRequest rather than on
// one specific token: the list is price-ordered and capped, so naming a token
// would make this a test of the cap rather than of the ordering.
func TestResolverReachesTheFortyGBTier(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 40, GpuCount: 4})

	idx := func(want string) int {
		for i, s := range got {
			if s == want {
				return i
			}
		}
		return -1
	}

	if idx("A100_PCIE_40GB") < 0 && idx("A100_SXM4_40GB") < 0 {
		t.Fatalf("no 40GB A100 reachable from a 40GB request; got %v", got)
	}
	for _, pair := range [][2]string{
		{"A100_PCIE_40GB", "A100_PCIE"},
		{"A100_SXM4_40GB", "A100_SXM4"},
	} {
		cheap, dear := idx(pair[0]), idx(pair[1])
		if cheap >= 0 && dear >= 0 && dear < cheap {
			t.Errorf("%s ranked above the cheaper %s: %v", pair[1], pair[0], got)
		}
	}
}

// An explicit --sku bypasses the resolver entirely, which is how the A/B pins
// each arm to a specific card. Both variants must be addressable that way even
// though the price-ordered list is capped.
func TestExplicitVariantSKUIsAddressable(t *testing.T) {
	for _, sku := range []string{"A100_SXM4_40GB", "A100_PCIE_40GB"} {
		if got := classifySKU(sku); got == "" {
			t.Errorf("%s: classifySKU returned empty, so the catalog does not know it", sku)
		}
	}
}

// Guard against a future row reusing a token, which would make LookupSKU
// return whichever came first and silently mis-price or mis-size a rental.
func TestCatalogTokensAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range skus {
		if seen[s.GpuName] {
			t.Errorf("duplicate SKU token %q", s.GpuName)
		}
		seen[s.GpuName] = true
	}
}

// captureQuery is defined in hostquality_test.go; this keeps the compiler
// honest if that file is ever renamed.
var _ = func() *http.ServeMux { return nil }
var _ = json.Marshal
