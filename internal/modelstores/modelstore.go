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

	// Mounts describes filesystem mounts the deployment needs. The
	// default HF-passthrough store leaves this empty (vLLM downloads
	// from HF inside the pod). A warm-cache store (RunPodVolumeStore)
	// fills it with the volume the pre-staged weights live on; the
	// Service stamps them onto Deployment.mounts and each deploy path
	// maps them onto its provider primitive.
	Mounts []Mount
}

// Mount is one filesystem mount a deployment's engine needs. It mirrors
// the provisionerv1.VolumeMount proto and is provider-agnostic: it
// names a mount, not a provider primitive. The three fields cover the
// two deploy paths without either leaking into the other.
//
//   - VolumeID is a provider-managed volume handle (a RunPod network
//     volume id today). The image-native deploy path attaches it by id.
//   - HostPath is a directory already present on the host, bind-mounted
//     by the sshdocker deploy path. Empty when VolumeID drives the mount.
//   - MountPath is the in-container path the engine sees. Always set; a
//     store points HF_HOME at it so the engine finds the staged weights.
//   - Provider namespaces VolumeID: a volume handle only means something
//     to the provider that issued it, so the deploy path refuses to
//     attach a mount whose provider does not match the replica's
//     placement provider. Empty skips the check (host-path binds are
//     not provider-scoped). Mirrors provisionerv1.VolumeMount.provider.
type Mount struct {
	VolumeID  string
	HostPath  string
	MountPath string
	Provider  string
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
