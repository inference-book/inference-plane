package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

func ptr(f float64) *float64 { return &f }

// The whole reason Vast gets its own tier: the marketplace filters on
// bw_nvlink server-side, so an intra-node request must reach the wire as a
// query constraint rather than being applied after paging through offers.
func TestFindOfferPushesFabricFilterServerSide(t *testing.T) {
	var gotQuery map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.Unmarshal([]byte(r.URL.Query().Get("q")), &gotQuery)
		writeJSON(w, bundlesResponse{Offers: nil})
	})
	p, _ := newTestProvider(t, mux)

	_, err := p.findOffer(context.Background(), "A100_SXM4", 2, 0,
		&provisionerv1.ResourceRequirements{
			FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
		})
	if err != nil {
		t.Fatalf("findOffer: %v", err)
	}

	bw, ok := gotQuery["bw_nvlink"].(map[string]any)
	if !ok {
		t.Fatalf("query carried no bw_nvlink filter: %v", gotQuery)
	}
	// >= 1, never >= 0. A zero floor matches unmeasured hosts and would
	// silently undo the filter.
	if got := bw["gte"].(float64); got < 1 {
		t.Errorf("bw_nvlink floor = %v, want >= 1 so unmeasured hosts are excluded", got)
	}
}

// The floor is expressed in Vast's unit (gigaBYTES), ours is gigabits, and
// the two differ by exactly the factor most likely to slip through review.
func TestFindOfferConvertsBandwidthToVastUnits(t *testing.T) {
	var gotQuery map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.Unmarshal([]byte(r.URL.Query().Get("q")), &gotQuery)
		writeJSON(w, bundlesResponse{Offers: nil})
	})
	p, _ := newTestProvider(t, mux)

	_, err := p.findOffer(context.Background(), "H100_SXM", 2, 0,
		&provisionerv1.ResourceRequirements{
			FabricScope:   provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
			MinFabricGbps: 7200, // 900 GB/s
		})
	if err != nil {
		t.Fatalf("findOffer: %v", err)
	}
	bw := gotQuery["bw_nvlink"].(map[string]any)
	if got := bw["gte"].(float64); got != 900 {
		t.Errorf("bw_nvlink floor = %v GB/s, want 900 (7200 gigabits / 8)", got)
	}
}

func TestFindOfferOmitsFilterWhenNoFabricRequested(t *testing.T) {
	var gotQuery map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.Unmarshal([]byte(r.URL.Query().Get("q")), &gotQuery)
		writeJSON(w, bundlesResponse{Offers: nil})
	})
	p, _ := newTestProvider(t, mux)

	if _, err := p.findOffer(context.Background(), "RTX_4090", 1, 0,
		&provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}
	if _, present := gotQuery["bw_nvlink"]; present {
		t.Errorf("unconstrained search sent a bw_nvlink filter: %v", gotQuery)
	}
}

// A bridge-capable card must survive the catalog pre-filter on Vast even
// though it fails the same requirement on RunPod. Dropping it here would
// discard exactly the hosts the measured tier exists to find: 3 of 24
// "A100 PCIE" offers in the 2026-08-09 probe had a real 275-300 GB/s link.
func TestMatchSKUsKeepsBridgeCapableForSearch(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{
		MinVramGb:   80,
		GpuCount:    2,
		FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	})
	if !slices.Contains(got, "A100_PCIE") {
		t.Errorf("A100_PCIE dropped before search; a bw_nvlink reading could still qualify it. got %v", got)
	}
	if !slices.Contains(got, "A100_SXM4") {
		t.Errorf("A100_SXM4 missing from intra-node candidates; got %v", got)
	}
}

func TestMatchSKUsStillDropsImpossibleFamilies(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{
		MinVramGb:   40,
		GpuCount:    2,
		FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	})
	for _, impossible := range []string{"L40S", "L40", "A40", "RTX_6000Ada"} {
		if slices.Contains(got, impossible) {
			t.Errorf("%q has no NVLink at all but survived the intra-node pre-filter; got %v", impossible, got)
		}
	}
}

func TestStampFabricFromMeasurement(t *testing.T) {
	tests := []struct {
		name       string
		gpuName    string
		bw         *float64
		wantScope  provisionerv1.FabricScope
		wantSource provisionerv1.FabricSource
		wantGbps   int32
	}{
		{
			// The headline case: the SKU says PCIe, the host says otherwise.
			name: "measured bridge on a card named PCIe", gpuName: "A100_PCIE", bw: ptr(300),
			wantScope:  provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
			wantSource: provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED, wantGbps: 2400,
		},
		{
			// The inverse: SXM boards always have NVLink, so a zero here is
			// Vast failing to measure, not the host lacking a link.
			name: "zero on an SXM board is not-measured", gpuName: "A100_SXM4", bw: ptr(0),
			wantScope:  provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			wantSource: provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN,
		},
		{
			name: "zero on a card with no link is a fact", gpuName: "L40S", bw: ptr(0),
			wantScope:  provisionerv1.FabricScope_FABRIC_SCOPE_NONE,
			wantSource: provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED,
		},
		{
			name: "no reading falls back to the declared tier", gpuName: "A100_SXM4", bw: nil,
			wantScope:  provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
			wantSource: provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED, wantGbps: 4800,
		},
		{
			name: "no reading on a bridge-capable card stays unknown", gpuName: "A100_PCIE", bw: nil,
			wantScope:  provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
			wantSource: provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hw := &provisionerv1.Hardware{GpuSku: tt.gpuName}
			stampFabric(hw, tt.gpuName, tt.bw)
			if hw.GetFabricScope() != tt.wantScope {
				t.Errorf("scope = %v, want %v", hw.GetFabricScope(), tt.wantScope)
			}
			if hw.GetFabricSource() != tt.wantSource {
				t.Errorf("source = %v, want %v", hw.GetFabricSource(), tt.wantSource)
			}
			if tt.wantGbps > 0 && hw.GetFabricGbps() != tt.wantGbps {
				t.Errorf("gbps = %d, want %d", hw.GetFabricGbps(), tt.wantGbps)
			}
		})
	}
}

// An absent bw_nvlink and a measured 0.0 must not collapse into each other.
// A plain float64 field would make them identical and silently mark every
// unmeasured host as having no fabric.
func TestAbsentReadingIsDistinctFromZeroOnTheWire(t *testing.T) {
	var withZero, withoutField offerSummary
	if err := json.Unmarshal([]byte(`{"id":1,"bw_nvlink":0.0}`), &withZero); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"id":1}`), &withoutField); err != nil {
		t.Fatal(err)
	}
	if withZero.BwNvlink == nil {
		t.Error("an explicit 0.0 decoded as absent")
	}
	if withoutField.BwNvlink != nil {
		t.Error("a missing field decoded as present")
	}
}

func TestEveryVastSKUHasAKnownFamily(t *testing.T) {
	for _, sku := range skus {
		if sku.Family == "" {
			t.Errorf("%q has no fabric.Family", sku.GpuName)
			continue
		}
		if !fabric.CouldSatisfy(sku.Family, provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE) &&
			!fabric.CouldSatisfy(sku.Family, provisionerv1.FabricScope_FABRIC_SCOPE_NONE) {
			t.Errorf("%q maps to family %q which the fabric catalog does not know", sku.GpuName, sku.Family)
		}
	}
}
