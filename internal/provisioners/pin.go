package provisioners

import (
	"context"
	"slices"
	"sort"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Warm-cache pinning: pre-stage a model onto a provider volume so later
// deploys mount weights instead of re-downloading from HuggingFace. A
// volume is a shared cache (many models, one per-region record); pinning
// is additive. These methods drive a provider's VolumeManager capability
// and record the result in State.Volumes.
//
// v0.2 ch9 (#191c): these are in-process Service methods, not gRPC RPCs.
// Pinning is a pre-stage step run before `iplane serve` (or when no
// daemon holds the state-dir lock); remote pinning over --service-url is
// a follow-up that lifts these onto DeploymentService.

// DefaultCacheMountPath is where a warm-cache volume attaches inside a
// pod, matched by the model_cache.mount_path deploy default so a staged
// model and the deployment that mounts it agree on HF_HOME's location.
const DefaultCacheMountPath = "/models"

// defaultCacheVolumeSizeGB is the size a per-region cache volume is
// created at when the operator does not specify one. Sized to hold a few
// 30-70B quantized models under one HF cache.
const defaultCacheVolumeSizeGB = 200

// cacheVolumeName is the deterministic shared-cache volume name for a
// region. Every model pinned to the same (provider, region) accumulates
// on this one volume rather than spawning a volume per model.
func cacheVolumeName(region string) string {
	return "iplane-cache-" + region
}

// PinModelRequest asks the Service to pre-stage a model onto a provider's
// per-region cache volume.
type PinModelRequest struct {
	Model    string
	Provider string
	Region   string
	HFToken  string
	SizeGB   int  // 0 -> defaultCacheVolumeSizeGB (only used when creating the volume)
	Force    bool // re-stage even if the registry already lists the model
}

// PinModelResult is the outcome of a pin. AlreadyStaged is true when the
// model was already recorded on the volume and staging was skipped.
type PinModelResult struct {
	Volume        *provisionerv1.Volume
	AlreadyStaged bool
}

// PinModel ensures a per-region cache volume exists, stages the model
// onto it (unless already present and not forced), and records the pin in
// the registry. The provider must implement VolumeManager.
//
// Staging runs outside the state lock (it spins a pod and can take
// minutes); only the quick registry upsert is done under Update.
func (s *Service) PinModel(ctx context.Context, req PinModelRequest) (*PinModelResult, error) {
	if req.Model == "" {
		return nil, status.Error(codes.InvalidArgument, "model is required")
	}
	if req.Region == "" {
		return nil, status.Error(codes.InvalidArgument, "region is required")
	}
	provider, ok := s.providers[req.Provider]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "provider %q is not configured", req.Provider)
	}
	vm, ok := provider.(VolumeManager)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented, "provider %q does not support model pinning (no persistent volumes)", req.Provider)
	}

	size := req.SizeGB
	if size <= 0 {
		size = defaultCacheVolumeSizeGB
	}
	ref, err := vm.EnsureVolume(ctx, VolumeSpec{Name: cacheVolumeName(req.Region), Region: req.Region, SizeGB: size})
	if err != nil {
		return nil, err
	}

	// Decide whether staging is needed from the registry's view.
	alreadyStaged := false
	if cur, rerr := s.store.Read(); rerr == nil {
		if v := cur.Volumes[ref.ID]; v != nil && !req.Force && slices.Contains(v.GetModels(), req.Model) {
			alreadyStaged = true
		}
	}
	if !alreadyStaged {
		if err := vm.StageModel(ctx, StageSpec{
			VolumeID:  ref.ID,
			Region:    req.Region,
			Model:     req.Model,
			MountPath: DefaultCacheMountPath,
			HFToken:   req.HFToken,
		}); err != nil {
			return nil, err
		}
	}

	// Upsert the registry record + record the model.
	var rec *provisionerv1.Volume
	err = s.store.Update(func(f *State) error {
		if f.Volumes == nil {
			f.Volumes = map[string]*provisionerv1.Volume{}
		}
		v := f.Volumes[ref.ID]
		if v == nil {
			v = &provisionerv1.Volume{
				Id:        ref.ID,
				Provider:  req.Provider,
				Region:    ref.Region,
				Name:      ref.Name,
				SizeGb:    int32(ref.SizeGB),
				MountPath: DefaultCacheMountPath,
				CreatedAt: timestamppb.New(s.clock()),
			}
			f.Volumes[ref.ID] = v
		}
		if !slices.Contains(v.Models, req.Model) {
			v.Models = append(v.Models, req.Model)
		}
		rec = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &PinModelResult{Volume: rec, AlreadyStaged: alreadyStaged}, nil
}

// ListVolumes returns the pinned cache volumes from the registry, sorted
// by id. An empty providerFilter returns all; otherwise only that
// provider's volumes.
func (s *Service) ListVolumes(_ context.Context, providerFilter string) ([]*provisionerv1.Volume, error) {
	state, err := s.store.Read()
	if err != nil {
		return nil, err
	}
	out := make([]*provisionerv1.Volume, 0, len(state.Volumes))
	for _, v := range state.Volumes {
		if providerFilter == "" || v.GetProvider() == providerFilter {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].GetId() < out[j].GetId() })
	return out, nil
}

// UnpinRequest removes a pin. With Model set, the model is dropped from
// the volume's registry entry (the staged files remain on the volume but
// stop being tracked). With Model empty, the whole volume is destroyed
// (provider DeleteVolume + registry removal).
type UnpinRequest struct {
	VolumeID string
	Model    string
}

// UnpinModel removes a model from a volume's registry entry, or destroys
// the whole volume when Model is empty. Returns the updated record, or
// nil when the volume was destroyed.
func (s *Service) UnpinModel(ctx context.Context, req UnpinRequest) (*provisionerv1.Volume, error) {
	if req.VolumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume_id is required")
	}
	cur, err := s.store.Read()
	if err != nil {
		return nil, err
	}
	rec := cur.Volumes[req.VolumeID]
	if rec == nil {
		return nil, status.Errorf(codes.NotFound, "no pinned volume %q", req.VolumeID)
	}

	// Whole-volume unpin: destroy the provider volume, then drop the record.
	if req.Model == "" {
		if provider, ok := s.providers[rec.GetProvider()]; ok {
			if vm, ok := provider.(VolumeManager); ok {
				if err := vm.DeleteVolume(ctx, req.VolumeID); err != nil {
					return nil, err
				}
			}
		}
		return nil, s.store.Update(func(f *State) error {
			delete(f.Volumes, req.VolumeID)
			return nil
		})
	}

	// Single-model unpin: registry-only. Files stay on the volume.
	var updated *provisionerv1.Volume
	err = s.store.Update(func(f *State) error {
		v := f.Volumes[req.VolumeID]
		if v == nil {
			return status.Errorf(codes.NotFound, "no pinned volume %q", req.VolumeID)
		}
		v.Models = slices.DeleteFunc(v.Models, func(m string) bool { return m == req.Model })
		updated = v
		return nil
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}
