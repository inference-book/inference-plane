package file

import (
	"sync"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// TestUpdate_ConcurrentSlotWritesDoNotLose is the regression for the
// lost-update bug that multi-replica-from-scratch deploys exposed. Under
// LockForLifetime (the iplane serve path), Update skips re-acquiring the
// flock, so without an in-process mutex two goroutines doing concurrent
// read-modify-write both read the same state and the last writer clobbers
// the other's mutation. Here N goroutines each fill a distinct slot of one
// deployment's engine_endpoints; every slot must survive.
//
// Mirrors the real failure: external's instant per-slot Deploy emits land
// near-simultaneously, unlike slow cloud provisioning.
func TestUpdate_ConcurrentSlotWritesDoNotLose(t *testing.T) {
	s, err := Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	release, err := s.LockForLifetime()
	if err != nil {
		t.Fatalf("LockForLifetime: %v", err)
	}
	defer release()

	const n = 8
	seed := func(f *provisioners.State) error {
		f.Deployments["d"] = &provisionerv1.Deployment{
			Id:              "d",
			EngineEndpoints: make([]string, n),
		}
		return nil
	}
	if err := s.Update(seed); err != nil {
		t.Fatalf("seed Update: %v", err)
	}

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			_ = s.Update(func(f *provisioners.State) error {
				f.Deployments["d"].EngineEndpoints[slot] = "ep"
				return nil
			})
		}(i)
	}
	wg.Wait()

	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	eps := got.Deployments["d"].GetEngineEndpoints()
	for i, ep := range eps {
		if ep == "" {
			t.Errorf("slot %d lost its write (concurrent Update clobbered it)", i)
		}
	}
}
