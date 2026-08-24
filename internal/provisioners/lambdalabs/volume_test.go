package lambdalabs

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
)

// fsMux serves Lambda's filesystem surface. Note the two spellings: the
// list is hyphenated and the create and delete are not, which is the whole
// of #432 and is transcribed here rather than smoothed over.
func fsMux(t *testing.T, existing []apiFilesystem, created *map[string]string, deleted *[]string, deleteStatus int) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/file-systems", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, filesystemListResponse{Data: existing})
	})
	mux.HandleFunc("/api/v1/filesystems", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		*created = req
		writeJSON(w, filesystemResponse{Data: apiFilesystem{
			ID: "new-id", Name: req["name"],
			MountPoint: "/lambda/nfs/" + req["name"],
			Region:     apiRegion{Name: req["region"]},
		}})
	})
	mux.HandleFunc("/api/v1/filesystems/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/filesystems/")
		if deleteStatus != 0 {
			http.Error(w, `{"error":{"code":"filesystems/filesystem-in-use","message":"Cannot delete a filesystem while it's mounted to an instance."}}`, deleteStatus)
			return
		}
		*deleted = append(*deleted, id)
		writeJSON(w, map[string]any{"data": map[string]any{"deleted_ids": []string{id}}})
	})
	return mux
}

// EnsureVolume finds before it creates, so pinning a second model to the
// same region reuses one cache rather than sprawling.
func TestEnsureVolume_ReusesAnExistingFilesystem(t *testing.T) {
	var created map[string]string
	var deleted []string
	p, _ := newTestProvider(t, fsMux(t, []apiFilesystem{{
		ID: "fs-1", Name: "iplane-cache-us-east-1",
		MountPoint: "/lambda/nfs/iplane-cache-us-east-1",
		Region:     apiRegion{Name: "us-east-1"},
	}}, &created, &deleted, 0))

	ref, err := p.EnsureVolume(t.Context(), provisioners.VolumeSpec{
		Name: "iplane-cache-us-east-1", Region: "us-east-1", SizeGB: 200,
	})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if created != nil {
		t.Errorf("created %v; the filesystem already existed", created)
	}
	if ref.ID != "iplane-cache-us-east-1" {
		t.Errorf("ref.ID = %q, want the name (see the handle note in volume.go)", ref.ID)
	}
	if ref.HostPath != "/lambda/nfs/iplane-cache-us-east-1" {
		t.Errorf("ref.HostPath = %q, want the vendor's own mount_point", ref.HostPath)
	}
}

// A filesystem in the wrong region is not this region's cache, however it
// is named. Lambda region-locks them and enforces it at launch.
func TestEnsureVolume_IgnoresAFilesystemInAnotherRegion(t *testing.T) {
	var created map[string]string
	var deleted []string
	p, _ := newTestProvider(t, fsMux(t, []apiFilesystem{{
		ID: "fs-1", Name: "iplane-cache-us-east-1",
		MountPoint: "/lambda/nfs/iplane-cache-us-east-1",
		Region:     apiRegion{Name: "us-west-1"},
	}}, &created, &deleted, 0))

	if _, err := p.EnsureVolume(t.Context(), provisioners.VolumeSpec{
		Name: "iplane-cache-us-east-1", Region: "us-east-1",
	}); err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if created == nil {
		t.Fatal("did not create; the existing filesystem is in another region")
	}
	if created["region"] != "us-east-1" {
		t.Errorf("created in %q, want us-east-1", created["region"])
	}
}

// Lambda takes no size at create and reports 8.0E from the guest, so the
// requested size is dropped rather than sent as a field the API would
// reject. Measured on hardware, 2026-08-24.
func TestEnsureVolume_SendsNoSize(t *testing.T) {
	var created map[string]string
	var deleted []string
	p, _ := newTestProvider(t, fsMux(t, nil, &created, &deleted, 0))

	ref, err := p.EnsureVolume(t.Context(), provisioners.VolumeSpec{
		Name: "iplane-cache-us-east-1", Region: "us-east-1", SizeGB: 200,
	})
	if err != nil {
		t.Fatalf("EnsureVolume: %v", err)
	}
	if _, sent := created["size"]; sent {
		t.Errorf("sent a size: %v", created)
	}
	if ref.SizeGB != 0 {
		t.Errorf("ref.SizeGB = %d, want 0; Lambda publishes no capacity to report", ref.SizeGB)
	}
}

// The name is the handle, so it has to satisfy Lambda's own pattern
// (^[a-zA-Z]+[0-9a-zA-Z-]*$, max 60) or the create fails after the
// operator has already waited.
func TestEnsureVolume_RefusesANameLambdaWouldReject(t *testing.T) {
	var created map[string]string
	var deleted []string
	p, _ := newTestProvider(t, fsMux(t, nil, &created, &deleted, 0))

	for _, bad := range []string{"1leading-digit", "has_underscore", "has.dot", ""} {
		if _, err := p.EnsureVolume(t.Context(), provisioners.VolumeSpec{
			Name: bad, Region: "us-east-1",
		}); err == nil {
			t.Errorf("accepted %q, which Lambda's filesystem-name pattern rejects", bad)
		}
	}
	if created != nil {
		t.Errorf("called the API with a name it would reject: %v", created)
	}
}

func TestListVolumes(t *testing.T) {
	var created map[string]string
	var deleted []string
	p, _ := newTestProvider(t, fsMux(t, []apiFilesystem{
		{ID: "fs-1", Name: "a", MountPoint: "/lambda/nfs/a", Region: apiRegion{Name: "us-east-1"}},
		{ID: "fs-2", Name: "b", MountPoint: "/lambda/nfs/b", Region: apiRegion{Name: "us-west-1"}},
	}, &created, &deleted, 0))

	vols, err := p.ListVolumes(t.Context())
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(vols) != 2 {
		t.Fatalf("got %d volumes, want 2", len(vols))
	}
	if vols[0].ID != "a" || vols[0].HostPath != "/lambda/nfs/a" {
		t.Errorf("vols[0] = %+v", vols[0])
	}
}

// DeleteVolume takes the handle, which is the name, and Lambda's delete
// takes the uuid. The adapter resolves one to the other rather than making
// the caller carry both.
func TestDeleteVolume_ResolvesTheNameToTheVendorID(t *testing.T) {
	var created map[string]string
	var deleted []string
	p, _ := newTestProvider(t, fsMux(t, []apiFilesystem{
		{ID: "fs-uuid-1", Name: "iplane-cache-us-east-1", Region: apiRegion{Name: "us-east-1"}},
	}, &created, &deleted, 0))

	if err := p.DeleteVolume(t.Context(), "iplane-cache-us-east-1"); err != nil {
		t.Fatalf("DeleteVolume: %v", err)
	}
	if len(deleted) != 1 || deleted[0] != "fs-uuid-1" {
		t.Errorf("deleted = %v, want [fs-uuid-1]", deleted)
	}
}

// A filesystem that is already gone is the end state DeleteVolume exists to
// reach, same idempotency rule as Terminate.
func TestDeleteVolume_AlreadyGoneIsSuccess(t *testing.T) {
	var created map[string]string
	var deleted []string
	p, _ := newTestProvider(t, fsMux(t, nil, &created, &deleted, 0))

	if err := p.DeleteVolume(t.Context(), "iplane-cache-us-east-1"); err != nil {
		t.Fatalf("DeleteVolume on a filesystem that is not there = %v, want nil", err)
	}
}

// "Still mounted" is a wait, not a failure, and it is reported as such
// because the instance may only be terminating rather than terminated.
// Observed on hardware: a DELETE seconds after a terminate returns
// filesystems/filesystem-in-use.
func TestDeleteVolume_InUseSaysSo(t *testing.T) {
	var created map[string]string
	var deleted []string
	p, _ := newTestProvider(t, fsMux(t, []apiFilesystem{
		{ID: "fs-uuid-1", Name: "iplane-cache-us-east-1", Region: apiRegion{Name: "us-east-1"}},
	}, &created, &deleted, http.StatusBadRequest))

	err := p.DeleteVolume(t.Context(), "iplane-cache-us-east-1")
	if err == nil {
		t.Fatal("expected an error while the filesystem is still mounted")
	}
	if !strings.Contains(err.Error(), "terminat") {
		t.Errorf("error should point at the instance still terminating: %v", err)
	}
}

func TestProviderSatisfiesVolumeManager(t *testing.T) {
	var _ provisioners.VolumeManager = (*Provider)(nil)
}

// The launch request is the only chance to attach a filesystem. The
// directory does not exist on the host until it is attached, and the
// sshdocker executor binds host paths at deploy time, which is minutes too
// late to ask.
func TestSpawn_AttachesTheVolumesTheSpecCarries(t *testing.T) {
	var launched map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ssh-keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, sshKeysResponse{Data: []apiSSHKey{{ID: "k1", Name: "GMac", PublicKey: "ssh-rsa AAAA"}}})
	})
	mux.HandleFunc("/api/v1/instance-operations/launch", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &launched)
		writeJSON(w, launchResponse{Data: struct {
			InstanceIDs []string `json:"instance_ids"`
		}{InstanceIDs: []string{"inst-1"}}})
	})
	mux.HandleFunc("/api/v1/instances/inst-1", func(w http.ResponseWriter, _ *http.Request) {
		body := apiInstance{ID: "inst-1", Name: "iplane-my-pod", Status: "booting"}
		body.InstanceType.Name = "gpu_1x_a10"
		writeJSON(w, instanceResponse{Data: body})
	})
	p, _ := newTestProvider(t, mux)

	spec := testSpec("my-pod")
	spec.VolumeIds = []string{"iplane-cache-us-east-1"}
	if _, err := p.Spawn(t.Context(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	names, _ := launched["file_system_names"].([]any)
	if len(names) != 1 || names[0] != "iplane-cache-us-east-1" {
		t.Errorf("file_system_names = %v, want the volume the Spec carried", names)
	}
}

// A cold deploy must not send an empty array. Lambda validates the field,
// and a deploy with nothing pinned should look exactly as it always did.
func TestSpawn_SendsNoFilesystemFieldWhenThereIsNoVolume(t *testing.T) {
	var launched map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ssh-keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, sshKeysResponse{Data: []apiSSHKey{{ID: "k1", Name: "GMac", PublicKey: "ssh-rsa AAAA"}}})
	})
	mux.HandleFunc("/api/v1/instance-operations/launch", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &launched)
		writeJSON(w, launchResponse{Data: struct {
			InstanceIDs []string `json:"instance_ids"`
		}{InstanceIDs: []string{"inst-1"}}})
	})
	mux.HandleFunc("/api/v1/instances/inst-1", func(w http.ResponseWriter, _ *http.Request) {
		body := apiInstance{ID: "inst-1", Name: "iplane-my-pod", Status: "booting"}
		body.InstanceType.Name = "gpu_1x_a10"
		writeJSON(w, instanceResponse{Data: body})
	})
	p, _ := newTestProvider(t, mux)

	if _, err := p.Spawn(t.Context(), testSpec("my-pod")); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if _, sent := launched["file_system_names"]; sent {
		t.Errorf("sent file_system_names on a cold deploy: %v", launched["file_system_names"])
	}
}
