package runpod

import (
	"context"

	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
)

// RunPod network-volume REST surface (rest.runpod.io/v1/networkvolumes).
// A network volume is datacenter-locked persistent storage; iplane uses
// it as a shared warm model cache (many models under one HF layout).
// These back the VolumeManager capability the warm-cache pin flow drives.

// networkVolumeCreate is the POST /networkvolumes request body. size is
// GB (RunPod range 0-4000); dataCenterId pins the volume's datacenter.
type networkVolumeCreate struct {
	Name         string `json:"name"`
	Size         int    `json:"size"`
	DataCenterID string `json:"dataCenterId"`
}

// networkVolume is the API's volume record. id is the handle passed as
// networkVolumeId when attaching the volume to a pod.
type networkVolume struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Size         int    `json:"size"`
	DataCenterID string `json:"dataCenterId"`
}

func (p *Provider) createNetworkVolume(ctx context.Context, name, dataCenterID string, sizeGB int) (networkVolume, error) {
	req, err := p.client.newReq("POST", "/networkvolumes", nil, networkVolumeCreate{
		Name:         name,
		Size:         sizeGB,
		DataCenterID: dataCenterID,
	})
	if err != nil {
		return networkVolume{}, wrapErr("create network volume", err)
	}
	vol, err := skhttp.Call[networkVolume](ctx, req, p.client.callOpts()...)
	if err != nil {
		return networkVolume{}, wrapErr("create network volume", err)
	}
	return vol, nil
}

func (p *Provider) listNetworkVolumes(ctx context.Context) ([]networkVolume, error) {
	req, err := p.client.newReq("GET", "/networkvolumes", nil, nil)
	if err != nil {
		return nil, wrapErr("list network volumes", err)
	}
	vols, err := skhttp.Call[[]networkVolume](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, wrapErr("list network volumes", err)
	}
	return vols, nil
}

func (p *Provider) deleteNetworkVolume(ctx context.Context, id string) error {
	req, err := p.client.newReq("DELETE", "/networkvolumes/"+id, nil, nil)
	if err != nil {
		return wrapErr("delete network volume", err)
	}
	if err := skhttp.CallVoid(ctx, req, p.client.callOpts()...); err != nil {
		return wrapErr("delete network volume", err)
	}
	return nil
}

// EnsureVolume finds a same-named volume in the region or creates one,
// satisfying provisioners.VolumeManager. Idempotent: pinning a second
// model to a region reuses the first pin's volume instead of creating a
// duplicate cache.
func (p *Provider) EnsureVolume(ctx context.Context, spec provisioners.VolumeSpec) (provisioners.VolumeRef, error) {
	existing, err := p.listNetworkVolumes(ctx)
	if err != nil {
		return provisioners.VolumeRef{}, err
	}
	for _, v := range existing {
		if v.Name == spec.Name && v.DataCenterID == spec.Region {
			return volumeRef(v), nil
		}
	}
	created, err := p.createNetworkVolume(ctx, spec.Name, spec.Region, spec.SizeGB)
	if err != nil {
		return provisioners.VolumeRef{}, err
	}
	return volumeRef(created), nil
}

// ListVolumes returns the account's network volumes as VolumeRefs.
func (p *Provider) ListVolumes(ctx context.Context) ([]provisioners.VolumeRef, error) {
	vols, err := p.listNetworkVolumes(ctx)
	if err != nil {
		return nil, err
	}
	refs := make([]provisioners.VolumeRef, 0, len(vols))
	for _, v := range vols {
		refs = append(refs, volumeRef(v))
	}
	return refs, nil
}

// DeleteVolume destroys a network volume (and everything staged on it).
func (p *Provider) DeleteVolume(ctx context.Context, volumeID string) error {
	return p.deleteNetworkVolume(ctx, volumeID)
}

func volumeRef(v networkVolume) provisioners.VolumeRef {
	return provisioners.VolumeRef{ID: v.ID, Name: v.Name, Region: v.DataCenterID, SizeGB: v.Size}
}

var _ provisioners.VolumeManager = (*Provider)(nil)
