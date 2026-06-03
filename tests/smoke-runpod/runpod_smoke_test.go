//go:build smoke_runpod

// Thin wrapper around tests/smoke-providers/core.go's shared smoke
// flow, specialized for RunPod. The per-provider file holds only
// the RunPod-specific config (env var name, SKU defaults, list
// filter shape); the shared core does the spawn + terminate +
// verify dance.
//
// Run:
//
//	export RUNPOD_API_KEY=...
//	make smoke-runpod              # read-only List (free)
//	RUNPOD_RENT=1 make smoke-runpod  # also rents + terminates a pod (~$0.05)
//
// The smoke flow's shared core moved in v0.2 ch7-beat3.11 follow-up
// when the Vast.ai smoke test came online -- two providers using a
// 90% identical test shape made the duplication obvious enough to
// extract.
package smoke_runpod

import (
	"os"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
	smokeproviders "github.com/inference-book/inference-plane/tests/smoke-providers"
)

func TestRunPod_List(t *testing.T) {
	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		t.Skip("RUNPOD_API_KEY not set; skipping (real-API smoke test)")
	}
	smokeproviders.RunList(t, runpod.New(runpod.NewClient(apiKey)))
}

func TestRunPod_SpawnAndTerminate(t *testing.T) {
	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		t.Skip("RUNPOD_API_KEY not set; skipping (real-API smoke test)")
	}
	sku := os.Getenv("RUNPOD_SKU")
	if sku == "" {
		// RunPod's gpu_name field uses the full display name. RTX
		// 3090 is cheap and reliably has secure-cloud capacity
		// across datacenters; RTX 4090 used to be the default but
		// gets capacity-bound (HTTP 500 "no instances available")
		// often enough that operators kept overriding it.
		sku = "NVIDIA GeForce RTX 3090"
	}
	// Region defaults empty: RunPod's dataCenterIds allowlist drifts
	// (e.g., US-CA-1 was retired in 2026-Q2) and a hardcoded default
	// breaks the smoke whenever the operator hasn't kept up. Empty
	// means RunPod schedules wherever; callers pin via RUNPOD_REGION
	// if they need a specific datacenter.
	region := os.Getenv("RUNPOD_REGION")
	smokeproviders.RunSpawnAndTerminate(t, smokeproviders.Config{
		Provider:         runpod.New(runpod.NewClient(apiKey)),
		ProviderName:     provisioners.ProviderRunPod,
		SKU:              sku,
		Region:           region,
		// CostEnv intentionally empty: preserves the v0.1 contract
		// that `make smoke-runpod` rents on every run when the key
		// is set. Operators who want the cost gate set
		// CostEnv="RUNPOD_RENT" via a future env-driven config.
		ExpectHourlyRate: true, // RunPod stamps costPerHr on the pod record
		// Server-side List filter by iplane-id catches the post-
		// spawn instance even before it transitions to ACTIVE.
		// RunPod's ?name= is exact-match, so we pass the full
		// stamped spec ID (not a "smoke-" prefix, which never
		// matched and silently returned 0 refs).
		ListFilterFn: func(id string) map[string]string {
			return map[string]string{provisioners.TagID: id}
		},
	})
}
