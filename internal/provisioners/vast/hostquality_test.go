package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// captureQuery serves an empty offer list and records the decoded `q` the
// search sent, which is where every server-side filter has to show up.
func captureQuery(t *testing.T, got *map[string]any) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		_ = json.Unmarshal([]byte(r.URL.Query().Get("q")), got)
		writeJSON(w, bundlesResponse{Offers: nil})
	})
	return mux
}

// Ordering by price alone rents hosts that are cheap for a reason. Measured on
// the live marketplace 2026-08-11 (RTX 3090, 1 GPU): the price-only cheapest
// offer advertised 357 Mbps down at reliability 0.9558, while the cheapest one
// clearing these floors advertised 1009 Mbps at 0.9878 for 12% more per hour.
// A rental that cannot pull its weights costs more than that difference.
func TestFindOfferPushesHostQualityFloorsServerSide(t *testing.T) {
	var gotQuery map[string]any
	p, _ := newTestProvider(t, captureQuery(t, &gotQuery))

	if _, err := p.findOffer(context.Background(), "RTX_3090", 1, 0,
		&provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}

	inet, ok := gotQuery["inet_down"].(map[string]any)
	if !ok {
		t.Fatalf("query carried no inet_down filter: %v", gotQuery)
	}
	if got := inet["gte"].(float64); got != DefaultMinInetDownMbps {
		t.Errorf("inet_down floor = %v, want %v", got, float64(DefaultMinInetDownMbps))
	}

	rel, ok := gotQuery["reliability2"].(map[string]any)
	if !ok {
		t.Fatalf("query carried no reliability2 filter: %v", gotQuery)
	}
	if got := rel["gte"].(float64); got != DefaultMinReliability {
		t.Errorf("reliability2 floor = %v, want %v", got, DefaultMinReliability)
	}

	// The floors bound eligibility, they do not replace the price ordering.
	// Dropping cheapest-first would quietly turn a cost-controlled search into
	// an expensive one.
	if _, present := gotQuery["order"]; !present {
		t.Errorf("search lost its cheapest-first ordering: %v", gotQuery)
	}
}

func TestWithHostQualityFloorOverrides(t *testing.T) {
	var gotQuery map[string]any
	srv := captureQuery(t, &gotQuery)
	p, _ := newTestProvider(t, srv)
	WithHostQualityFloor(250, 0.5)(p)

	if _, err := p.findOffer(context.Background(), "RTX_3090", 1, 0,
		&provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}
	if got := gotQuery["inet_down"].(map[string]any)["gte"].(float64); got != 250 {
		t.Errorf("inet_down floor = %v, want 250", got)
	}
	if got := gotQuery["reliability2"].(map[string]any)["gte"].(float64); got != 0.5 {
		t.Errorf("reliability2 floor = %v, want 0.5", got)
	}
}

// The escape hatch has to actually remove the constraint. A floor of 0 that
// still reached the wire as `gte: 0` would match every host including the
// unmeasured ones, which is a different behaviour from not filtering and would
// make thin-capacity searches behave unpredictably.
func TestWithHostQualityFloorZeroOmitsFilter(t *testing.T) {
	var gotQuery map[string]any
	p, _ := newTestProvider(t, captureQuery(t, &gotQuery))
	WithHostQualityFloor(0, 0)(p)

	if _, err := p.findOffer(context.Background(), "RTX_3090", 1, 0,
		&provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("findOffer: %v", err)
	}
	if _, present := gotQuery["inet_down"]; present {
		t.Errorf("disabled inet_down floor still reached the wire: %v", gotQuery)
	}
	if _, present := gotQuery["reliability2"]; present {
		t.Errorf("disabled reliability2 floor still reached the wire: %v", gotQuery)
	}
}

// The floors are the one constraint the operator did not type, so an empty
// result must not read as "the marketplace has no capacity". Without this the
// obvious next move is to go hunting for a capacity problem that does not
// exist.
func TestSpawnNoOfferNamesTheQualityFloors(t *testing.T) {
	var gotQuery map[string]any
	p, _ := newTestProvider(t, captureQuery(t, &gotQuery))

	_, err := p.Spawn(context.Background(), &provisionerv1.Spec{
		Id: "vq",
		Requirements: &provisionerv1.ResourceRequirements{
			Sku: "RTX_3090",
		},
	})
	if err == nil {
		t.Fatal("Spawn succeeded with no offers; want an error")
	}
	msg := err.Error()
	for _, want := range []string{"inet_down", "reliability2", "IPLANE_VAST_MIN_INET_DOWN_MBPS"} {
		if !strings.Contains(msg, want) {
			t.Errorf("no-offer error does not mention %q, so the floors are invisible to the operator: %s", want, msg)
		}
	}
}
