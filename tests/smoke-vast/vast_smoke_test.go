//go:build smoke_vast

// Thin wrapper around tests/smoke-providers/core.go's shared smoke
// flow, specialized for Vast.ai. The per-provider file holds only
// the Vast-specific config (env var name, SKU defaults, list
// filter shape); the shared core does the spawn + terminate +
// verify dance.
//
// Run:
//
//	export VAST_API_KEY=...
//	make smoke-vast              # read-only List (free)
//	VAST_RENT=1 make smoke-vast  # also rents + terminates an RTX 3090
package smoke_vast

import (
	"os"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/vast"
	smokeproviders "github.com/inference-book/inference-plane/tests/smoke-providers"
)

func TestVast_List(t *testing.T) {
	apiKey := os.Getenv("VAST_API_KEY")
	if apiKey == "" {
		t.Skip("VAST_API_KEY not set; skipping (real-API smoke test)")
	}
	smokeproviders.RunList(t, vast.New(vast.NewClient(apiKey)))
}

func TestVast_SpawnAndTerminate(t *testing.T) {
	apiKey := os.Getenv("VAST_API_KEY")
	if apiKey == "" {
		t.Skip("VAST_API_KEY not set; skipping (real-API smoke test)")
	}
	sku := os.Getenv("VAST_SKU")
	if sku == "" {
		sku = "RTX_3090" // cheap, widely available on the marketplace
	}
	smokeproviders.RunSpawnAndTerminate(t, smokeproviders.Config{
		Provider:     vast.New(vast.NewClient(apiKey)),
		ProviderName: provisioners.ProviderVast,
		SKU:          sku,
		Region:       os.Getenv("VAST_REGION"), // empty = unpinned
		CostEnv:      "VAST_RENT",
		// Vast's adapter doesn't populate hourly_rate_usd yet (the
		// dph_total comes back on the offer search but isn't
		// surfaced on the Instance). Leave ExpectHourlyRate false.
		ListFilter: map[string]string{"label-prefix": "iplane-"},
	})
}
