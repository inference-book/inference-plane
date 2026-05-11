//go:build smoke_runpod

// Package smoke_runpod actually hits the real RunPod API. Run only
// when you mean it -- this test provisions a small pod, costs a few
// cents per run, and the budget guardrail in the iplane CLI is NOT
// in this path (the adapter trusts the Service layer above it).
//
// Run:
//
//	export RUNPOD_API_KEY=...
//	make smoke-runpod
//
// The dedicated build tag (smoke_runpod, not plain smoke) keeps this
// out of every other test target so an operator running `make smoke`
// to check the HTTP layer cannot accidentally provision a real pod.
package smoke_runpod

import (
	"context"
	"os"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
)

const (
	// Smallest reliably-available SKU + region pair. Override via env
	// if your account is in a different region or has different
	// capacity at the moment.
	defaultRegion = "US-CA-1"
	defaultSKU    = "NVIDIA GeForce RTX 4090"
)

func TestRunPod_SpawnAndTerminate(t *testing.T) {
	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		t.Skip("RUNPOD_API_KEY not set; skipping (this test costs real money)")
	}
	region := envOr("RUNPOD_REGION", defaultRegion)
	sku := envOr("RUNPOD_SKU", defaultSKU)

	provider := runpod.New(runpod.NewClient(apiKey))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	spec := &provisionerv1.Spec{
		Id:       "iplane-smoke-" + shortStamp(),
		Provider: provisioners.ProviderRunPod,
		Region:   region,
		Gpu:      &provisionerv1.GpuSpec{Sku: sku},
		Tags: map[string]string{
			provisioners.TagID:       "iplane-smoke-" + shortStamp(),
			provisioners.TagOperator: "default",
			"purpose":                "iplane-runpod-smoke-test",
		},
	}

	t.Logf("Spawning %s in %s ...", sku, region)
	inst, err := provider.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Logf("Spawned pod %s @ $%.4f/hr (state=%s)", inst.GetProviderId(), inst.GetHourlyRateUsd(), inst.GetState())

	// Always attempt termination, even if assertions below fail.
	defer func() {
		t.Logf("Terminating pod %s ...", inst.GetProviderId())
		termCtx, termCancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer termCancel()
		if err := provider.Terminate(termCtx, inst.GetProviderId()); err != nil {
			t.Errorf("Terminate failed: %v (manual cleanup required: pod id %s)", err, inst.GetProviderId())
		}
	}()

	if inst.GetProviderId() == "" {
		t.Fatal("expected non-empty provider id")
	}
	if inst.GetHourlyRateUsd() <= 0 {
		t.Errorf("expected positive hourly rate, got %v", inst.GetHourlyRateUsd())
	}

	// Confirm List sees it.
	refs, err := provider.List(ctx, map[string]string{provisioners.TagID: spec.GetId()})
	if err != nil {
		t.Errorf("List: %v", err)
	}
	found := false
	for _, ref := range refs {
		if ref.GetProviderId() == inst.GetProviderId() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("List did not return the pod we just spawned (refs=%d)", len(refs))
	}
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func shortStamp() string {
	return time.Now().UTC().Format("20060102t150405")
}
