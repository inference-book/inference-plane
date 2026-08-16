package provisioners_test

import (
	"context"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// Reading the pin registry must work while another process holds the write
// lock, because the process holding it is normally the daemon and an operator
// asking what is staged should not have to stop their control plane (#307).
//
// Safe rather than merely convenient: the store writes through a temp file and
// an atomic rename, so a reader without the flock sees either the old file or
// the new one. It can be stale. It cannot be torn.
func TestListVolumesWorksWhileTheWriteLockIsHeld(t *testing.T) {
	dir := t.TempDir()

	holder, err := file.Open(dir, "daemon")
	if err != nil {
		t.Fatalf("open holder store: %v", err)
	}
	if err := holder.Update(func(f *provisioners.State) error {
		f.Volumes["vol-1"] = &provisionerv1.Volume{
			Id: "vol-1", Provider: "runpod", Region: "US-TX-3", Models: []string{"m"},
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := holder.LockForLifetime(); err != nil {
		t.Fatalf("holder could not take the lock: %v", err)
	}

	reader, err := file.Open(dir, "reader")
	if err != nil {
		t.Fatalf("open reader store while locked: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{local.New()}, reader, "reader")

	resp, err := svc.ListVolumes(context.Background(), &provisionerv1.ListVolumesRequest{})
	if err != nil {
		t.Fatalf("ListVolumes under a held lock: %v", err)
	}
	if len(resp.GetVolumes()) != 1 || resp.GetVolumes()[0].GetId() != "vol-1" {
		t.Errorf("got %v, want the seeded volume", resp.GetVolumes())
	}
}

// The provider filter exists because a volume handle only means something to
// the provider that issued it.
func TestListVolumesFiltersByProvider(t *testing.T) {
	dir := t.TempDir()
	store, err := file.Open(dir, "op")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Update(func(f *provisioners.State) error {
		f.Volumes["a"] = &provisionerv1.Volume{Id: "a", Provider: "runpod"}
		f.Volumes["b"] = &provisionerv1.Volume{Id: "b", Provider: "vast"}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{local.New()}, store, "op")

	resp, err := svc.ListVolumes(context.Background(),
		&provisionerv1.ListVolumesRequest{Provider: "vast"})
	if err != nil {
		t.Fatalf("ListVolumes: %v", err)
	}
	if len(resp.GetVolumes()) != 1 || resp.GetVolumes()[0].GetId() != "b" {
		t.Errorf("got %v, want only the vast volume", resp.GetVolumes())
	}
}

// The write verbs keep the lock. The fix must not leak into them: pinning
// stages weights and a second writer racing that is exactly what the flock is
// for.
func TestPinStillRequiresTheWriteLock(t *testing.T) {
	dir := t.TempDir()
	holder, err := file.Open(dir, "daemon")
	if err != nil {
		t.Fatalf("open holder: %v", err)
	}
	if _, err := holder.LockForLifetime(); err != nil {
		t.Fatalf("holder lock: %v", err)
	}

	second, err := file.Open(dir, "second")
	if err != nil {
		t.Fatalf("open second: %v", err)
	}
	if _, err := second.LockForLifetime(); err == nil {
		t.Error("a second writer took the lock; the pin path's guard is gone")
	}
}
