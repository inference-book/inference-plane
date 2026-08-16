package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// DefaultDiskGb is a typical default for the tier, not a ceiling the card
// imposes. Disk is an independent create param: Spawn reads min_disk_gb
// straight off the requirements and hands it to findOffer, which pushes it
// into the offer search. So a catalog-stage disk filter rejects hardware that
// would have served, and a 72B FP8 asking for 150 GB (what
// examples/08-scaling-30b sets) finds nothing at all on a marketplace full of
// capacity. RunPod removed the same filter during the 72B cold-start work.
// See issue 281.
func TestMinDiskDoesNotFilterTheCatalog(t *testing.T) {
	unconstrained := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 80})
	withDisk := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 80, MinDiskGb: 150})

	if len(withDisk) == 0 {
		t.Fatalf("min_disk_gb=150 matched no SKU; unconstrained matched %v", unconstrained)
	}
	if len(withDisk) != len(unconstrained) {
		t.Errorf("min_disk_gb=150 changed the match set: got %v, want %v", withDisk, unconstrained)
	}
}

// The quieter half of the same bug. At min_disk_gb=100 the filter dropped
// every 80 GB SKU and left the resolver starting at H100 NVL, so an operator
// who asked for a large card and a big disk silently got a more expensive
// tier rather than an error.
func TestMinDiskDoesNotEscalatePriceTier(t *testing.T) {
	got := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 80, MinDiskGb: 100})

	if len(got) == 0 {
		t.Fatal("min_disk_gb=100 matched no SKU")
	}
	cheapest := LookupSKU(got[0])
	if cheapest == nil {
		t.Fatalf("matched gpu %q not in catalog", got[0])
	}
	if cheapest.VRAMGb != 80 {
		t.Errorf("cheapest match is %s at %d GB VRAM, want an 80 GB part; the disk filter escalated the tier",
			got[0], cheapest.VRAMGb)
	}
}

// Vast reports cpu_ram per offer, so a RAM floor belongs in the marketplace
// query where the real host is judged, not in a catalog filter comparing a
// tier estimate. Same move the disk floor already made, and the reason the
// Vast catalog projection no longer carries system RAM at all (#283).
func TestMinRamPushedIntoTheOfferSearch(t *testing.T) {
	var gotQuery map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.Unmarshal([]byte(r.URL.Query().Get("q")), &gotQuery)
		writeJSON(w, bundlesResponse{Offers: nil})
	})
	p, _ := newTestProvider(t, mux)

	_, err := p.findOffer(context.Background(), "A100_SXM4", 2, 0,
		&provisionerv1.ResourceRequirements{MinRamGb: 256})
	if err != nil {
		t.Fatalf("findOffer: %v", err)
	}

	ram, ok := gotQuery["cpu_ram"].(map[string]any)
	if !ok {
		t.Fatalf("query carried no cpu_ram floor: %v", gotQuery)
	}
	// Vast reports cpu_ram in MB.
	if got := ram["gte"].(float64); got != 256000 {
		t.Errorf("cpu_ram floor = %v, want 256000 MB", got)
	}
}

// An unstated RAM requirement must not become a floor of zero, which would be
// harmless here but is the shape that silently narrows a search elsewhere.
func TestNoRamFloorWhenUnstated(t *testing.T) {
	var gotQuery map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.Unmarshal([]byte(r.URL.Query().Get("q")), &gotQuery)
		writeJSON(w, bundlesResponse{Offers: nil})
	})
	p, _ := newTestProvider(t, mux)

	if _, err := p.findOffer(context.Background(), "A100_SXM4", 2, 0,
		&provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}

	if _, present := gotQuery["cpu_ram"]; present {
		t.Errorf("query carried a cpu_ram filter for a request that stated none: %v", gotQuery)
	}
}

// The catalog stage must stop guessing once the offer search can answer.
// Filtering on a tier estimate alongside a real per-host floor can only
// wrongly exclude, which is exactly what the disk filter did.
func TestCatalogStageNoLongerFiltersOnRam(t *testing.T) {
	unconstrained := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 80})
	withRAM := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 80, MinRamGb: 4096})

	if len(withRAM) != len(unconstrained) {
		t.Errorf("an unreachable min_ram_gb changed the catalog match set: got %v, want %v",
			withRAM, unconstrained)
	}
}
