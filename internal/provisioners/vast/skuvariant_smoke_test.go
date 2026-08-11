//go:build smoke_vast

// Real-API check that the A100 variant SKUs select the card they name.
// Read-only and free: it searches, it never rents.
//
// The unit tests assert against a fake that echoes whatever we send, so they
// prove the query is well-formed and nothing else. Whether "A100 SXM4" plus a
// gpu_ram band actually separates the 40 GB part from the 80 GB one is a claim
// about Vast's data, and only the live marketplace can answer it.
//
// Run: VAST_API_KEY=... make smoke-vast-offers
package vast

import (
	"context"
	"os"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

func TestVastRealAPI_VariantSKUsSelectTheRightCard(t *testing.T) {
	apiKey := os.Getenv("VAST_API_KEY")
	if apiKey == "" {
		t.Skip("VAST_API_KEY not set; skipping (real-API smoke test)")
	}

	// Quality floors off: this test is about which CARD comes back, and a
	// thin-capacity moment on the quality-filtered subset would turn it into a
	// flake about host reliability instead.
	p := New(NewClient(apiKey), WithHostQualityFloor(0, 0))

	for _, tc := range []struct {
		sku            string
		wantMinMB      int
		wantMaxMB      int
		mustNotContain string
	}{
		{"A100_SXM4_40GB", 35000, 50000, "an 80GB card"},
		{"A100_SXM4", 70000, 200000, "a 40GB card"},
		{"A100_PCIE_40GB", 35000, 50000, "an 80GB card"},
		{"A100_PCIE", 70000, 200000, "a 40GB card"},
	} {
		t.Run(tc.sku, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			offer, err := p.findOffer(ctx, tc.sku, 1, 0, &provisionerv1.ResourceRequirements{})
			if err != nil {
				t.Fatalf("findOffer(%s): %v", tc.sku, err)
			}
			if offer == nil {
				// Legitimate: the marketplace does not always have every
				// variant rentable. Skipping beats renting to find out.
				t.Skipf("no %s offer available right now; nothing to assert", tc.sku)
			}

			ram := offer.GpuRAM
			t.Logf("%s -> offer %d, gpu_ram=%d MB, $%.4f/hr", tc.sku, offer.ID, ram, offer.DphTotal)

			if ram < tc.wantMinMB || ram > tc.wantMaxMB {
				t.Errorf("%s returned a card with gpu_ram=%d MB, outside [%d,%d]: the search matched %s",
					tc.sku, ram, tc.wantMinMB, tc.wantMaxMB, tc.mustNotContain)
			}
		})
	}
}
