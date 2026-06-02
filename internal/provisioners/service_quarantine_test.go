package provisioners_test

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// TestService_Quarantine_AddsToSet: a single Quarantine call appends
// the instance id to unhealthy_instance_ids. The endpoint URL stays
// in engine_endpoints (non-destructive quarantine).
func TestService_Quarantine_AddsToSet(t *testing.T) {
	svc, store, _ := newSvcWithDeploy(t)
	seedDeploy(t, store, "d", []string{"a", "b"}, []string{"http://a", "http://b"})

	if err := svc.Quarantine("d", "a"); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	dep := readDep(t, store, "d")
	if got := dep.GetUnhealthyInstanceIds(); len(got) != 1 || got[0] != "a" {
		t.Errorf("unhealthy_instance_ids = %v, want [a]", got)
	}
	if got := dep.GetEngineEndpoints(); len(got) != 2 || got[0] != "http://a" {
		t.Errorf("engine_endpoints should survive quarantine, got %v", got)
	}
}

// TestService_Quarantine_Idempotent: calling Quarantine twice on the
// same (deploy, instance) keeps the set at one entry. The health-poll
// loop relies on this: it may re-fire Quarantine across racing ticks
// before the source snapshot reflects the prior call.
func TestService_Quarantine_Idempotent(t *testing.T) {
	svc, store, _ := newSvcWithDeploy(t)
	seedDeploy(t, store, "d", []string{"a"}, []string{"http://a"})

	for range 3 {
		if err := svc.Quarantine("d", "a"); err != nil {
			t.Fatalf("Quarantine: %v", err)
		}
	}
	dep := readDep(t, store, "d")
	if got := dep.GetUnhealthyInstanceIds(); len(got) != 1 {
		t.Errorf("expected single entry after repeated Quarantine, got %v", got)
	}
}

// TestService_Restore_RemovesFromSet: Restore removes the instance
// from the set; the deployment record otherwise unchanged.
func TestService_Restore_RemovesFromSet(t *testing.T) {
	svc, store, _ := newSvcWithDeploy(t)
	seedDeploy(t, store, "d", []string{"a", "b", "c"}, []string{"http://a", "http://b", "http://c"})
	_ = svc.Quarantine("d", "b")

	if err := svc.Restore("d", "b"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	dep := readDep(t, store, "d")
	if got := dep.GetUnhealthyInstanceIds(); len(got) != 0 {
		t.Errorf("expected empty set after Restore, got %v", got)
	}
}

// TestService_Restore_NoOp: Restore on a never-quarantined replica is
// a no-op, not an error. Same shape callers want from Quarantine.
func TestService_Restore_NoOp(t *testing.T) {
	svc, store, _ := newSvcWithDeploy(t)
	seedDeploy(t, store, "d", []string{"a"}, []string{"http://a"})

	if err := svc.Restore("d", "a"); err != nil {
		t.Fatalf("Restore on healthy replica should be no-op, got %v", err)
	}
}

// TestService_Quarantine_MissingDeploy: the health-poll loop can race
// against Destroy -- by the time Quarantine fires, the deployment is
// gone. Method silently no-ops rather than surfacing NotFound, since
// the caller is internal and has no recovery action.
func TestService_Quarantine_MissingDeploy(t *testing.T) {
	svc, _, _ := newSvcWithDeploy(t)
	if err := svc.Quarantine("does-not-exist", "a"); err != nil {
		t.Errorf("Quarantine on missing deploy should no-op, got %v", err)
	}
	if err := svc.Restore("does-not-exist", "a"); err != nil {
		t.Errorf("Restore on missing deploy should no-op, got %v", err)
	}
}

// TestService_Quarantine_EmptyArgs: defensive zero-value rejection.
// Empty deploy id or instance id is a no-op (returns nil) -- there
// is nothing meaningful to mutate.
func TestService_Quarantine_EmptyArgs(t *testing.T) {
	svc, _, _ := newSvcWithDeploy(t)
	if err := svc.Quarantine("", "a"); err != nil {
		t.Errorf("empty deploy id should no-op, got %v", err)
	}
	if err := svc.Quarantine("d", ""); err != nil {
		t.Errorf("empty instance id should no-op, got %v", err)
	}
	if err := svc.Restore("", "a"); err != nil {
		t.Errorf("Restore empty deploy: got %v", err)
	}
}

// seedDeploy inserts a RUNNING deployment record with the supplied
// instance/endpoint lists. Bypasses CreateDeployment so the test
// doesn't need to wire executors / providers / model stores --
// Quarantine/Restore operate on the state record directly.
func seedDeploy(t *testing.T, store storeUpdater, depID string, instanceIDs, endpoints []string) {
	t.Helper()
	if err := store.Update(func(f *provisioners.State) error {
		f.Deployments[depID] = &provisionerv1.Deployment{
			Id:              depID,
			State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			InstanceIds:     instanceIDs,
			EngineEndpoints: endpoints,
			EngineEndpoint:  firstNonEmpty(endpoints),
		}
		return nil
	}); err != nil {
		t.Fatalf("seedDeploy: %v", err)
	}
}

// readDep snapshots the deployment record straight from the state
// file for assertions. Bypasses DescribeDeployment so the test
// reads exactly what was persisted (no service-side mutations).
func readDep(t *testing.T, store storeReader, depID string) *provisionerv1.Deployment {
	t.Helper()
	state, err := store.Read()
	if err != nil {
		t.Fatalf("store.Read: %v", err)
	}
	dep, ok := state.Deployments[depID]
	if !ok {
		t.Fatalf("deployment %q not found", depID)
	}
	return dep
}

func firstNonEmpty(ss []string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// storeUpdater + storeReader are the narrow contracts the helpers
// need from *file.Store. Declared inline so the helpers compile
// without exposing the full Store surface in test code.
type storeUpdater interface {
	Update(func(*provisioners.State) error) error
}
type storeReader interface {
	Read() (*provisioners.State, error)
}
