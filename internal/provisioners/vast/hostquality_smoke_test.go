//go:build smoke_vast

// Real-API check that the marketplace-quality floors are still honoured by
// Vast's search. Read-only and free: it searches, it never rents.
//
// This lives in package vast rather than tests/smoke-vast/ because findOffer
// is unexported, and exporting a search entry point purely so a test could
// reach it would add public API that nothing else wants.
//
// Run:
//
//	export VAST_API_KEY=...
//	make smoke-vast-offers
package vast

import (
	"context"
	"os"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// The floors are pushed server-side, so "the filter works" is a claim about
// Vast's behaviour, not ours, and the unit tests deliberately cannot check it:
// they assert against a fake that echoes whatever we send.
//
// The assertion that matters is the last one. If Vast renames inet_down, stops
// honouring it, or changes its units, the query still returns 200 with a full
// list of offers and our search silently reverts to price-only ordering. That
// is the failure this test exists to catch, and the only way to see it is to
// read the values back off the offer that was returned.
func TestVastRealAPI_QualityFloorsExcludeWeakHosts(t *testing.T) {
	apiKey := os.Getenv("VAST_API_KEY")
	if apiKey == "" {
		t.Skip("VAST_API_KEY not set; skipping (real-API smoke test)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	p := New(NewClient(apiKey))
	offer, err := p.findOffer(ctx, "RTX_3090", 1, 0, &provisionerv1.ResourceRequirements{})
	if err != nil {
		t.Fatalf("findOffer against the live marketplace: %v", err)
	}
	if offer == nil {
		// Not a failure. The floors are allowed to exclude everything when the
		// marketplace is thin, and a test that rented on empty capacity would
		// be worse than one that says so.
		t.Skip("no RTX 3090 offer currently clears the default floors; nothing to assert")
	}

	t.Logf("picked offer id=%d $%.4f/hr inet_down=%.0f Mbps reliability2=%.4f",
		offer.ID, offer.DphTotal, offer.InetDown, offer.Reliability2)

	if offer.InetDown < DefaultMinInetDownMbps {
		t.Errorf("offer %d has inet_down %.0f Mbps, below the %v floor the search asked for; the filter is no longer being honoured",
			offer.ID, offer.InetDown, float64(DefaultMinInetDownMbps))
	}
	if offer.Reliability2 < DefaultMinReliability {
		t.Errorf("offer %d has reliability2 %.4f, below the %v floor the search asked for; the filter is no longer being honoured",
			offer.ID, offer.Reliability2, DefaultMinReliability)
	}
}
