package state

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestOpen_CreatesDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "iplane")
	s, err := Open(dir, "default")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("Open did not create dir: %v", err)
	}
	if s.Path() != filepath.Join(dir, "state.json") {
		t.Errorf("Path() = %q, want state.json under dir", s.Path())
	}
}

func TestRead_EmptyFileReturnsDefault(t *testing.T) {
	s := newStore(t)
	f, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if f.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", f.SchemaVersion, SchemaVersion)
	}
	if f.Backend != BackendLocalFile {
		t.Errorf("Backend = %q, want %q", f.Backend, BackendLocalFile)
	}
	if f.OperatorID != "default" {
		t.Errorf("OperatorID = %q, want default", f.OperatorID)
	}
	if len(f.Instances) != 0 {
		t.Errorf("Instances should be empty, got %d", len(f.Instances))
	}
}

func TestUpdate_WriteThenRead(t *testing.T) {
	s := newStore(t)
	want := &provisionerv1.Instance{
		Id:       "my-pod",
		Provider: "local",
		State:    provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
	}
	if err := s.Update(func(f *File) error {
		f.Instances["my-pod"] = want
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	inst, ok := got.Instances["my-pod"]
	if !ok {
		t.Fatal("instance my-pod missing after round-trip")
	}
	if inst.GetId() != "my-pod" {
		t.Errorf("Id = %q, want my-pod", inst.GetId())
	}
	if inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("State = %v, want ACTIVE", inst.GetState())
	}
}

func TestUpdate_AbortOnFnError(t *testing.T) {
	s := newStore(t)
	wantErr := "boom"
	err := s.Update(func(f *File) error {
		f.Instances["should-not-persist"] = &provisionerv1.Instance{Id: "ghost"}
		return errFromString(wantErr)
	})
	if err == nil || err.Error() != wantErr {
		t.Errorf("Update error = %v, want %q", err, wantErr)
	}
	got, _ := s.Read()
	if _, ok := got.Instances["should-not-persist"]; ok {
		t.Error("ghost record persisted despite fn error")
	}
}

func TestUpdate_AtomicWrite_NoTornFiles(t *testing.T) {
	s := newStore(t)
	// Fill with one instance, then verify after Update the file is
	// well-formed JSON (not a half-written torn file). The atomicity
	// comes from temp-file-then-rename in writeToDisk; this test asserts
	// the file is always valid by reading it back after every Update.
	for i := range 25 {
		id := "pod-" + itoa(i)
		err := s.Update(func(f *File) error {
			f.Instances[id] = &provisionerv1.Instance{Id: id, State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE}
			return nil
		})
		if err != nil {
			t.Fatalf("Update %d: %v", i, err)
		}
		f, err := s.Read()
		if err != nil {
			t.Fatalf("Read after Update %d: %v", i, err)
		}
		if len(f.Instances) != i+1 {
			t.Fatalf("after Update %d: expected %d instances, got %d", i, i+1, len(f.Instances))
		}
	}
}

func TestUpdate_FlockSerializesConcurrentWriters(t *testing.T) {
	// Two goroutines racing Update on the same Store. The flock should
	// serialize them so the final state has BOTH increments, not one
	// (which would happen if read-modify-write was non-atomic).
	s := newStore(t)
	if err := s.Update(func(f *File) error {
		f.Instances["counter"] = &provisionerv1.Instance{Id: "counter"}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var counter atomic.Int32
	var wg sync.WaitGroup
	const n = 50
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Update(func(f *File) error {
				existing := f.Instances["counter"]
				existing.HourlyRateUsd++
				counter.Add(1)
				return nil
			})
			if err != nil {
				t.Errorf("concurrent Update: %v", err)
			}
		}()
	}
	wg.Wait()
	if int(counter.Load()) != n {
		t.Errorf("update calls = %d, want %d", counter.Load(), n)
	}
	f, _ := s.Read()
	if int(f.Instances["counter"].GetHourlyRateUsd()) != n {
		t.Errorf("HourlyRateUsd = %v, want %d -- flock did not serialize updates", f.Instances["counter"].GetHourlyRateUsd(), n)
	}
}

func TestRead_ForwardCompat_UnknownFieldsTolerated(t *testing.T) {
	// A v0.2 writer adds a new top-level field; the v0.1 reader must
	// not choke. protojson with DiscardUnknown=true on instances and
	// json's default top-level tolerance covers this.
	s := newStore(t)
	raw := `{
  "schema_version": "1",
  "backend": "local-file",
  "operator_id": "default",
  "future_field": "from a newer iplane",
  "instances": {
    "my-pod": {
      "id": "my-pod",
      "state": "INSTANCE_STATE_ACTIVE",
      "future_instance_field": "also from a newer iplane"
    }
  }
}`
	if err := os.WriteFile(s.Path(), []byte(raw), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	f, err := s.Read()
	if err != nil {
		t.Fatalf("Read should tolerate unknown fields: %v", err)
	}
	if inst, ok := f.Instances["my-pod"]; !ok || inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("instance not parsed correctly: %+v", inst)
	}
}

// TestLock_FDSurvivesGC is the regression test for the flock GC bug.
//
// The original lock() returned an int (the raw file descriptor) and let
// the *os.File go out of scope. Go's runtime registers a finalizer on
// every *os.File that closes the underlying FD when the value becomes
// unreachable. Closing the FD silently released the flock AND -- worse
// -- handed the FD slot back to the OS, which could reuse it for a
// completely unrelated socket (a gRPC stream, say). unlock's later
// syscall.Close on the same numeric FD would then tear down whatever
// now lived there, manifesting as "unexpected EOF" on the gRPC client.
//
// The fix is to return *os.File from lock() so the caller's stack
// holds a reference for the entire locked section. This test guards
// against anyone "simplifying" the signature back to int.
func TestLock_FDSurvivesGC(t *testing.T) {
	s := newStore(t)
	f, err := s.lock()
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	defer s.unlock(f)

	// Force several GC cycles. If lock() ever stops keeping the
	// *os.File reachable, the finalizer would close the FD here.
	for i := 0; i < 16; i++ {
		runtime.GC()
	}

	// Proof: the FD is still ours. A closed-then-recycled FD would
	// either fail this write or write to something unrelated.
	if _, err := f.Write([]byte("alive\n")); err != nil {
		t.Fatalf("lock FD died across GC -- finalizer must have closed it: %v", err)
	}
}

// TestUpdate_SurvivesGCPressure exercises the public Update entry point
// under GC pressure. A regression in lock()'s lifetime management would
// surface as a stray Close on a recycled FD; with the fix in place,
// every Update returns clean and the stored values are consistent.
func TestUpdate_SurvivesGCPressure(t *testing.T) {
	s := newStore(t)
	for i := 0; i < 32; i++ {
		err := s.Update(func(f *File) error {
			// Force GC inside the locked section. If lock's *os.File
			// were collectable here, this is exactly when the
			// finalizer would fire and close the lock FD.
			runtime.GC()
			runtime.GC()
			f.Instances["k-"+itoa(i)] = &provisionerv1.Instance{
				Id:    "k-" + itoa(i),
				State: provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Update #%d: %v", i, err)
		}
	}
	out, err := s.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got := len(out.Instances); got != 32 {
		t.Errorf("instance count = %d, want 32", got)
	}
}

// errString is the cheapest way to make an error from a literal in tests.
type errString string

func (e errString) Error() string { return string(e) }
func errFromString(s string) error { return errString(s) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
