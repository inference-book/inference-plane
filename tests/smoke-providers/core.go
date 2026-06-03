// Package smokeproviders holds the shared real-API smoke-test core
// that every per-provider package (tests/smoke-runpod, smoke-vast,
// future smoke-lambdalabs, ...) wraps thinly. The per-provider files
// keep their //go:build smoke_<name> tags so the existing
// `make smoke-<name>` targets stay scoped; only the config struct
// and test runner here are shared.
//
// Why a shared core: every cloud's spawn + terminate test has the
// same shape -- skip-if-key-missing, gate-by-cost-env, Spawn, defer
// Terminate, assert via List. Duplicating that shape across N
// providers means a fix in one (timeout, cleanup contract, log
// format) drifts away from the others. One core, N thin wrappers,
// one shape to evolve.
package smokeproviders

import (
	"context"
	"os"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// Config is what a per-provider smoke test passes into the shared
// RunSpawnAndTerminate. Most fields have sensible defaults
// (CostEnv, Timeout, RentTimeout); only Provider + ProviderName +
// SKU are typically required.
type Config struct {
	// Provider is the constructed adapter. The wrapper test
	// builds this from the per-provider client (e.g.,
	// `runpod.New(runpod.NewClient(apiKey))`).
	Provider provisioners.Provider

	// ProviderName is the provisioners.Provider<Name> constant
	// for the adapter (provisioners.ProviderRunPod, etc).
	ProviderName string

	// SKU is the GPU type to request. Format per-provider:
	// "NVIDIA GeForce RTX 4090" for RunPod, "RTX_3090" for Vast.
	SKU string

	// Region is the optional region pin. Empty = unpinned (the
	// provider's scheduler picks). Some providers require it.
	Region string

	// CostEnv names the env var that gates rental (the test
	// otherwise costs money). The standard name is the
	// provider's all-caps "${PROVIDER}_RENT". When unset, the
	// rental test is skipped and the smoke run is free.
	CostEnv string

	// Timeout caps the whole spawn+verify phase. Defaults to
	// 3 minutes which covers cold-start image pulls on most
	// providers.
	Timeout time.Duration

	// TerminateTimeout caps the terminate call. Defaults to
	// 60 seconds.
	TerminateTimeout time.Duration

	// ExpectHourlyRate, when true, asserts the spawned Instance
	// has a positive hourly_rate_usd populated. RunPod stamps this
	// from the pod's costPerHr; some adapters don't (yet). False
	// is the safe default.
	ExpectHourlyRate bool

	// ListFilter is the tag filter passed to provider.List for
	// the post-spawn verification. RunPod uses
	// {iplane-id: <stamp>}; Vast uses {label-prefix: iplane-}.
	// When nil, the test skips the post-spawn List check.
	ListFilter map[string]string
}

// RunSpawnAndTerminate is the load-bearing smoke check: rent a
// small instance from the real provider, verify the response shape,
// then terminate. Always tries terminate in a defer so a failed
// assertion doesn't leak a paid rental.
//
// The wrapper test typically does:
//
//	apiKey := os.Getenv(provisioners.ProviderAPIKeyEnv(name))
//	if apiKey == "" { t.Skip(...) }
//	smokeproviders.RunSpawnAndTerminate(t, smokeproviders.Config{
//	    Provider:     provider,
//	    ProviderName: name,
//	    SKU:          os.Getenv("SOMETHING") OR fallback,
//	    CostEnv:      "RUNPOD_RENT",
//	})
func RunSpawnAndTerminate(t *testing.T, cfg Config) {
	t.Helper()
	// CostEnv gates rental only when set: empty means "no gate"
	// (the wrapper opts in to the spawn unconditionally; the
	// pre-flight VAST_API_KEY / RUNPOD_API_KEY check in the
	// wrapper is the only guard). Non-empty CostEnv requires the
	// operator to explicitly opt into spending money each run.
	if cfg.CostEnv != "" && os.Getenv(cfg.CostEnv) != "1" {
		t.Skipf("rental test costs money; set %s=1 to enable", cfg.CostEnv)
	}
	if cfg.Provider == nil {
		t.Fatal("Config.Provider is required")
	}
	if cfg.SKU == "" {
		t.Fatal("Config.SKU is required")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 3 * time.Minute
	}
	if cfg.TerminateTimeout == 0 {
		cfg.TerminateTimeout = 60 * time.Second
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	stamp := time.Now().UTC().Format("20060102t150405")
	spec := &provisionerv1.Spec{
		Id:       "smoke-" + stamp,
		Provider: cfg.ProviderName,
		Region:   cfg.Region,
		Requirements: &provisionerv1.ResourceRequirements{
			Sku:      cfg.SKU,
			GpuCount: 1,
		},
		Tags: map[string]string{
			provisioners.TagID:       "smoke-" + stamp,
			provisioners.TagOperator: "default",
			"purpose":                "iplane-" + cfg.ProviderName + "-smoke-test",
		},
	}

	t.Logf("Spawning %s on %s ...", cfg.SKU, cfg.ProviderName)
	inst, err := cfg.Provider.Spawn(ctx, spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	t.Logf("Spawned instance %s (state=%s, hourly_rate=$%.4f/hr)",
		inst.GetProviderId(), inst.GetState(), inst.GetHourlyRateUsd())

	// Always attempt termination, even if assertions below fail.
	defer func() {
		t.Logf("Terminating instance %s ...", inst.GetProviderId())
		termCtx, termCancel := context.WithTimeout(context.Background(), cfg.TerminateTimeout)
		defer termCancel()
		if err := cfg.Provider.Terminate(termCtx, inst.GetProviderId()); err != nil {
			t.Errorf("Terminate FAILED: %v -- MANUAL CLEANUP REQUIRED for instance %s on %s",
				err, inst.GetProviderId(), cfg.ProviderName)
		}
	}()

	if inst.GetProviderId() == "" {
		t.Fatal("expected non-empty provider id after Spawn")
	}
	if cfg.ExpectHourlyRate && inst.GetHourlyRateUsd() <= 0 {
		t.Errorf("expected positive hourly rate, got %v (adapter not stamping cost?)", inst.GetHourlyRateUsd())
	}

	// Optional post-spawn List check: confirm the rented instance
	// shows up in the provider's account view via tag filter.
	if cfg.ListFilter == nil {
		return
	}
	refs, err := cfg.Provider.List(ctx, cfg.ListFilter)
	if err != nil {
		t.Errorf("List after Spawn: %v", err)
		return
	}
	found := false
	for _, ref := range refs {
		if ref.GetProviderId() == inst.GetProviderId() {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("List did not return the instance we just spawned (refs=%d, filter=%v)",
			len(refs), cfg.ListFilter)
	}
}

// RunList runs the free read-only smoke check: List returns
// without error. Useful as the "is the auth header right and is
// the v1 endpoint shape correct" gate before the rental test.
//
// Wrapper test pattern:
//
//	smokeproviders.RunList(t, provider)
func RunList(t *testing.T, p provisioners.Provider) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	refs, err := p.List(ctx, map[string]string{})
	if err != nil {
		t.Fatalf("List against real %s API failed: %v", p.Name(), err)
	}
	t.Logf("List returned %d instances on this account", len(refs))
	for i, r := range refs {
		if i >= 5 {
			t.Logf("  ... %d more", len(refs)-5)
			break
		}
		t.Logf("  [%d] provider_id=%s provider_state=%s tags=%v",
			i, r.GetProviderId(), r.GetProviderState(), r.GetTags())
	}
}
