package lambdalabs

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"

	"github.com/inference-book/inference-plane/internal/provisioners"
	skhttp "github.com/panyam/servicekit/http"
)

// Lambda Labs persistent filesystems, which back the VolumeManager
// capability the warm-cache pin flow drives.
//
// **The collection is spelled two ways and only one of them takes a
// write.** `GET /api/v1/file-systems` is hyphenated; `POST
// /api/v1/filesystems` and `DELETE /api/v1/filesystems/{id}` are not. A 405
// fired at the hyphenated path is what wrote this capability off for four
// weeks in docs/design/0004 (#432). The constants below keep the two apart
// so nobody has to remember which is which.
//
// **The handle is the name, not the uuid.** VolumeRef.ID is opaque to
// everything except this adapter, and the name is what the rest of Lambda's
// API actually accepts: `file_system_names` at launch takes names, and the
// mount point is derived from the name. Only DELETE wants the uuid, so this
// file resolves name to uuid there and nowhere else. Using the uuid as the
// handle would mean a lookup on the far more common launch path instead.
const (
	pathFileSystemsRead  = "/api/v1/file-systems"
	pathFileSystemsWrite = "/api/v1/filesystems"
)

// apiRegion is the region block Lambda repeats across records.
type apiRegion struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// apiFilesystem is one persistent filesystem.
//
// MountPoint is derived by Lambda from the name (`/lambda/nfs/<name>`) and
// is read back rather than reconstructed, so the adapter is not asserting a
// path layout the vendor could change. IsInUse reports whether an instance
// currently has it mounted, which is a different question from whether it
// exists.
type apiFilesystem struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	MountPoint string    `json:"mount_point"`
	Region     apiRegion `json:"region"`
	IsInUse    bool      `json:"is_in_use"`
}

type filesystemListResponse struct {
	Data []apiFilesystem `json:"data"`
}

type filesystemResponse struct {
	Data apiFilesystem `json:"data"`
}

// filesystemName is Lambda's own bound on the field: a letter, then letters,
// digits and hyphens, at most 60 characters. Transcribed from the published
// schema.
var filesystemName = regexp.MustCompile(`^[a-zA-Z]+[0-9a-zA-Z-]{0,59}$`)

// EnsureVolume satisfies provisioners.VolumeManager: find the region's cache
// filesystem or create it.
//
// **VolumeSpec.SizeGB is ignored, deliberately.** Lambda takes no size at
// create and the guest reports 8.0E of capacity, so there is nothing to
// request and nothing honest to report back. Sending the field would fail
// validation; echoing the requested figure into VolumeRef would put a number
// in the operator's `iplane model ls` that no measurement supports. Measured
// on hardware 2026-08-24; see docs/design/0010.
//
// The name is checked before the call rather than after, because a rejected
// name surfaces as a create failure partway through a pin the operator has
// already waited on.
//
// Idempotent: matching on (name, region) rather than name alone, since
// Lambda region-locks a filesystem and enforces that at launch. A filesystem
// of the right name in the wrong region is not this region's cache.
func (p *Provider) EnsureVolume(ctx context.Context, spec provisioners.VolumeSpec) (provisioners.VolumeRef, error) {
	if !filesystemName.MatchString(spec.Name) {
		return provisioners.VolumeRef{}, provisioners.NewProviderError(p.Name(), "ensure-volume",
			fmt.Errorf("filesystem name %q is not one Lambda accepts: it must start with a letter and carry only letters, digits and hyphens, at most 60 characters", spec.Name), 0)
	}
	if spec.Region == "" {
		return provisioners.VolumeRef{}, provisioners.NewProviderError(p.Name(), "ensure-volume",
			errors.New("region is required; a Lambda filesystem is region-locked and can only be mounted by an instance in the same region"), 0)
	}

	existing, err := p.listFilesystems(ctx)
	if err != nil {
		return provisioners.VolumeRef{}, wrapErr("ensure-volume:list", err)
	}
	for _, fs := range existing {
		if fs.Name == spec.Name && fs.Region.Name == spec.Region {
			return volumeRef(fs), nil
		}
	}

	req, err := p.client.newReq(http.MethodPost, pathFileSystemsWrite, nil, map[string]any{
		"name":   spec.Name,
		"region": spec.Region,
	})
	if err != nil {
		return provisioners.VolumeRef{}, wrapErr("ensure-volume:create", err)
	}
	resp, err := skhttp.Call[filesystemResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return provisioners.VolumeRef{}, wrapErr("ensure-volume:create", err)
	}
	return volumeRef(resp.Data), nil
}

// ListVolumes satisfies provisioners.VolumeManager.
func (p *Provider) ListVolumes(ctx context.Context) ([]provisioners.VolumeRef, error) {
	all, err := p.listFilesystems(ctx)
	if err != nil {
		return nil, wrapErr("list-volumes", err)
	}
	out := make([]provisioners.VolumeRef, 0, len(all))
	for _, fs := range all {
		out = append(out, volumeRef(fs))
	}
	return out, nil
}

// DeleteVolume satisfies provisioners.VolumeManager, taking the handle this
// adapter issues, which is the filesystem name.
//
// Two behaviours worth knowing, both observed on hardware.
//
// A filesystem that is not there is success, the same idempotency rule
// Terminate carries: the end state this call exists to reach is already the
// end state.
//
// A filesystem that is still mounted is a **wait**, and the error says so.
// Lambda refuses the delete while any instance has it, and an instance that
// has merely been asked to terminate still counts. So the honest report is
// "not yet" rather than a bare 400, because the caller's next move is to
// wait rather than to investigate.
func (p *Provider) DeleteVolume(ctx context.Context, volumeID string) error {
	all, err := p.listFilesystems(ctx)
	if err != nil {
		return wrapErr("delete-volume:list", err)
	}
	var id string
	for _, fs := range all {
		if fs.Name == volumeID || fs.ID == volumeID {
			id = fs.ID
			break
		}
	}
	if id == "" {
		return nil // already gone
	}

	req, err := p.client.newReq(http.MethodDelete, pathFileSystemsWrite+"/"+id, nil, nil)
	if err != nil {
		return wrapErr("delete-volume", err)
	}
	if err := skhttp.CallVoid(ctx, req, p.client.callOpts()...); err != nil {
		wrapped := wrapErr("delete-volume", err)
		if errors.Is(wrapped, provisioners.ErrNotFound) {
			return nil
		}
		if isFilesystemInUse(err) {
			return provisioners.NewProviderError(p.Name(), "delete-volume",
				fmt.Errorf("filesystem %q is still mounted; an instance may only be terminating rather than terminated, so retry once it is gone", volumeID), 0)
		}
		return wrapped
	}
	return nil
}

// StageModel satisfies provisioners.VolumeManager and is not implemented
// yet, so `iplane model pin` gets this far and stops here.
//
// Everything the download needs is now in place: the filesystem is created
// by EnsureVolume, attached by naming it in the launch request, and visible
// to a container through the sshdocker executor's bind. What is missing is
// the throwaway machine that does the downloading, and on Lambda that is the
// expensive part rather than a detail. Lambda sells no CPU-only tier, so the
// staging box is a GPU at $1.29/hr against RunPod's ~$0.06/hr CPU pod, for an
// operation that is pure network. That deserves its own change and its own
// decision about whether to warn the operator, rather than riding in on this
// one. Tracked on #436.
func (p *Provider) StageModel(_ context.Context, spec provisioners.StageSpec) error {
	return provisioners.NewProviderError(p.Name(), "stage-model",
		fmt.Errorf("staging %q onto a Lambda filesystem is not implemented yet (#436); the volume exists and mounts, but nothing downloads onto it. Stage by hand over SSH, or pin on a provider with a CPU tier", spec.Model), 0)
}

// listFilesystems reads the collection. Note the hyphenated path: the read
// and the writes live at different spellings.
func (p *Provider) listFilesystems(ctx context.Context) ([]apiFilesystem, error) {
	req, err := p.client.newReq(http.MethodGet, pathFileSystemsRead, nil, nil)
	if err != nil {
		return nil, err
	}
	resp, err := skhttp.Call[filesystemListResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// volumeRef renders a filesystem as the shared handle.
//
// SizeGB stays zero rather than carrying a figure: Lambda neither takes a
// size nor publishes one, and a fabricated number is worse than an absent
// one wherever an operator reads it back.
func volumeRef(fs apiFilesystem) provisioners.VolumeRef {
	return provisioners.VolumeRef{
		ID:       fs.Name,
		Name:     fs.Name,
		Region:   fs.Region.Name,
		HostPath: fs.MountPoint,
	}
}

// isFilesystemInUse reports Lambda's own refusal code for deleting a
// mounted filesystem.
func isFilesystemInUse(err error) bool {
	var httpErr *skhttp.HTTPError
	if !errors.As(err, &httpErr) {
		return false
	}
	_, code := parseErrorBody(httpErr.Body)
	return code == "filesystems/filesystem-in-use"
}
