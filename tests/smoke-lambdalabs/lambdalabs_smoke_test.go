//go:build smoke_lambdalabs

// Thin wrapper around tests/smoke-providers/core.go's shared smoke
// flow, specialized for Lambda Labs. Mirrors the smoke-vast and
// smoke-runpod wrappers; the per-provider file holds only the
// Lambda-specific config.
//
// Run:
//
//	export LAMBDA_API_KEY=...
//	make smoke-lambdalabs                   # read-only List (free)
//	LAMBDA_RENT=1 make smoke-lambdalabs     # also rents + terminates an A10
//
// Lambda's cheapest available SKU when capacity exists is
// gpu_1x_a10 at $1.29/hr (~$0.02 for a 60s rental). Capacity
// fluctuates by region; the wrapper passes us-east-1 (where A10s
// were available at smoke-write time, 2026-06).
package smoke_lambdalabs

import (
	"os"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/lambdalabs"
	smokeproviders "github.com/inference-book/inference-plane/tests/smoke-providers"
)

func TestLambdaLabs_List(t *testing.T) {
	apiKey := os.Getenv("LAMBDA_API_KEY")
	if apiKey == "" {
		t.Skip("LAMBDA_API_KEY not set; skipping (real-API smoke test)")
	}
	smokeproviders.RunList(t, lambdalabs.New(lambdalabs.NewClient(apiKey)))
}

func TestLambdaLabs_SpawnAndTerminate(t *testing.T) {
	apiKey := os.Getenv("LAMBDA_API_KEY")
	if apiKey == "" {
		t.Skip("LAMBDA_API_KEY not set; skipping (real-API smoke test)")
	}
	sku := os.Getenv("LAMBDA_SKU")
	if sku == "" {
		sku = "gpu_1x_a10"
	}
	region := os.Getenv("LAMBDA_REGION")
	if region == "" {
		region = "us-east-1"
	}
	smokeproviders.RunSpawnAndTerminate(t, smokeproviders.Config{
		Provider:         lambdalabs.New(lambdalabs.NewClient(apiKey)),
		ProviderName:     provisioners.ProviderLambdaLabs,
		SKU:              sku,
		Region:           region,
		CostEnv:          "LAMBDA_RENT",
		ExpectHourlyRate: true, // Lambda stamps price_cents_per_hour on the instance type
		ListFilterFn: func(_ string) map[string]string {
			return map[string]string{"name-prefix": "iplane-"}
		},
	})
}
