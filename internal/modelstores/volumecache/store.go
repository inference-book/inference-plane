// Package volumecache implements a warm-cache ModelStore that mounts a
// pre-staged volume so the engine loads weights off it instead of
// re-downloading from HuggingFace on every deploy.
//
// It is provider-agnostic: it stamps an opaque Mount (a provider volume
// handle and/or a host path, plus the in-container attach path) and
// redirects HF_HOME at that path. Which deploy path interprets the
// mount -- the RunPod adapter onto networkVolumeId, the sshdocker
// executor onto a `docker run -v` bind -- is decided at deploy time, not
// here. There is no provider-specific branch in this package, which is
// why it carries no provider name.
//
// This is the *mount* half of the warm-cache story. Populating the
// volume (download-once into it) is a separate concern; a Store here
// assumes the volume already holds the model's HuggingFace hub cache.
// On a cache miss the engine still reaches HF normally (HF_HOME is
// redirected, not forced offline), so an unpopulated volume degrades to
// a cold deploy rather than failing.
package volumecache

import (
	"context"
	"fmt"
	"maps"
	"path"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
)

// Store is a decorator: it delegates spec validation to a base
// ModelStore (typically huggingface.Store), then adds the configured
// mount and an HF_HOME override pointing at the staged cache on it.
type Store struct {
	base  modelstores.ModelStore
	mount modelstores.Mount
}

// New returns a Store that stamps mount onto every resolved deployment
// and resolves specs through base for validation. base must be non-nil;
// pass modelstores.Passthrough{} to skip validation. mount.MountPath
// must be set (it is where the engine reads weights); mount.VolumeID
// and/or mount.HostPath name the volume for whichever deploy path
// attaches it.
func New(base modelstores.ModelStore, mount modelstores.Mount) *Store {
	return &Store{base: base, mount: mount}
}

// hfHomeSubdir is the directory under the mount that holds the
// HuggingFace cache layout (hub/models--Org--Name/...). The population
// step writes here; HF_HOME points here so the engine finds the staged
// snapshot.
const hfHomeSubdir = "hf"

// Resolve validates the spec through the base store, then augments the
// result with the mount and an HF_HOME override so the engine loads the
// staged weights. The base store's EngineModelArg (the HF id) is
// preserved unchanged -- redirecting HF_HOME is what makes the cache
// hit, not rewriting --model. Base EnvOverrides (e.g. HF_TOKEN) are
// kept; HF_HOME is added on top.
func (s *Store) Resolve(ctx context.Context, spec string) (modelstores.Resolved, error) {
	resolved, err := s.base.Resolve(ctx, spec)
	if err != nil {
		return modelstores.Resolved{}, err
	}

	env := make(map[string]string, len(resolved.EnvOverrides)+1)
	maps.Copy(env, resolved.EnvOverrides)
	env["HF_HOME"] = path.Join(s.mount.MountPath, hfHomeSubdir)
	resolved.EnvOverrides = env

	resolved.Mounts = append(resolved.Mounts, s.mount)
	return resolved, nil
}

// Architecture forwards to the base store unchanged. Where a model's
// weights are staged says nothing about the model's shape, so a warm
// cache has nothing to add to the answer.
//
// Forwarding is manual because this is a decorator rather than an
// embedding, and that is the trap worth naming: satisfying ModelStore is
// enough to compile and enough to pass this package's own tests, while
// silently dropping every optional capability the base could answer. It
// dropped this one for a release. A daemon with the warm cache enabled
// reported a hub-backed store as unable to describe a model, which is
// the Chapter 9 configuration disabling the Chapter 12 pre-flight (#324).
//
// A base that genuinely cannot answer gets ErrNoArchitectureSource
// rather than an invented reply, so "no hub behind this" still reads as
// capability-absent at the RPC surface.
func (s *Store) Architecture(ctx context.Context, req *provisionerv1.DescribeModelRequest) (*provisionerv1.DescribeModelResponse, error) {
	src, ok := s.base.(modelstores.ArchitectureSource)
	if !ok {
		return nil, fmt.Errorf("%w: the warm cache wraps a store that has no hub to ask", modelstores.ErrNoArchitectureSource)
	}
	return src.Architecture(ctx, req)
}

var (
	_ modelstores.ModelStore         = (*Store)(nil)
	_ modelstores.ArchitectureSource = (*Store)(nil)
)
