// Package modelstores defines how iplane transforms operator-supplied
// model specifications (HF ids, future: URLs, local paths, network-
// volume references) into the concrete form the engine actually
// consumes.
//
// v0.1's only impl is huggingface.Store, which is essentially a
// pass-through with pre-flight validation: catch typos / gated-access
// errors against huggingface.co/api/models before paying for a pod
// spin. v0.2 adds CachedStore (a decorator that pre-populates a
// network volume on first resolve, mounts the cached path on
// subsequent ones) and RunPodVolumeStore (the cached side itself).
//
// The interface is deliberately tiny so v0.2's wrappers can be drop-
// in: a CachedStore is just `ModelStore` + a `next ModelStore` and a
// volume manager.
package modelstores

import (
	"context"
	"fmt"
)

// ModelStore transforms an operator-supplied model spec into a
// Resolved description the engine can consume.
//
// Resolve is called by the provisioners.Service at CreateDeployment
// time, before any provider call. A non-nil error aborts the deploy
// with codes.InvalidArgument (the model is unusable; no point
// spinning a pod).
type ModelStore interface {
	Resolve(ctx context.Context, spec string) (Resolved, error)
}

// Resolved is what the Service uses to populate the deployment's
// engine-facing fields. v0.1 only fills EngineModelArg and (optionally)
// EnvOverrides; the other fields exist so v0.2's CachedStore /
// RunPodVolumeStore have a clean place to extend without proto
// changes.
type Resolved struct {
	// EngineModelArg is what the engine receives in --model
	// (vLLM, TGI, Triton with vllm backend all accept the same format).
	// For HF passthrough: same as the input spec. For CachedStore:
	// the local mount path the engine should load from.
	EngineModelArg string

	// EnvOverrides are merged into the deployment's pod env on
	// CreateDeployment. v0.1 uses this for HF_TOKEN propagation;
	// v0.2 may set HF_HOME / TRANSFORMERS_CACHE / etc. when caching
	// is active.
	EnvOverrides map[string]string

	// Mounts describes filesystem mounts the deployment needs. v0.1
	// is always empty (vLLM downloads from HF inside the pod). v0.2's
	// CachedStore fills this with the network-volume path the
	// pre-populated cache lives at.
	Mounts []Mount
}

// Mount is a future-shape placeholder. v0.1 never produces these.
// v0.2's RunPodVolumeStore will fill in (VolumeID, MountPath).
type Mount struct {
	VolumeID  string
	MountPath string
}

// Passthrough is the no-op ModelStore: returns the input spec
// unchanged, no env, no mounts. Used by tests and as the fallback
// when validation is disabled (--skip-model-validation). Production
// callers use huggingface.Store.
type Passthrough struct{}

func (Passthrough) Resolve(_ context.Context, spec string) (Resolved, error) {
	if spec == "" {
		return Resolved{}, fmt.Errorf("model spec is required")
	}
	return Resolved{EngineModelArg: spec}, nil
}

// Ensure Passthrough satisfies the interface.
var _ ModelStore = Passthrough{}
