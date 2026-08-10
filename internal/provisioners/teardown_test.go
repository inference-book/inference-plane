package provisioners_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

func instanceStates(t *testing.T, store interface {
	Read() (*provisioners.State, error)
}, ids ...string) map[string]provisionerv1.InstanceState {
	t.Helper()
	st, err := store.Read()
	if err != nil {
		t.Fatalf("store.Read: %v", err)
	}
	out := map[string]provisionerv1.InstanceState{}
	for _, id := range ids {
		if inst, ok := st.Instances[id]; ok {
			out[id] = inst.GetState()
		}
	}
	return out
}

// Issue 228. Destroying a deployment created directly at N>1 replicas used to
// mark it TERMINATED while leaving every replica instance ACTIVE, because the
// teardown read the singular instance_id that recordCreateSlots leaves empty
// for multi-replica. On a real provider that is N GPUs still billing after
// the operator believes the deployment is gone.
func TestDestroyDeploymentTerminatesEveryReplica(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 3)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	if _, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"}); err != nil {
		t.Fatalf("DestroyDeployment: %v", err)
	}

	states := instanceStates(t, store, "my-llama-r0", "my-llama-r1", "my-llama-r2")
	if len(states) != 3 {
		t.Fatalf("expected 3 replica records, got %d", len(states))
	}
	for id, got := range states {
		if got != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
			t.Errorf("%s = %s, want TERMINATED; a live replica after destroy is a billing leak", id, got)
		}
	}

	st, _ := store.Read()
	if got := st.Deployments["my-llama"].GetState(); got != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("deployment state = %s, want TERMINATED", got)
	}
}

// One replica refusing to die must not strand the others, and must not be
// reported as success. The deployment stays TERMINATING so the operator keeps
// a handle to retry.
func TestDestroyDeploymentPartialFailureStillTearsDownTheRest(t *testing.T) {
	var destroyed []string
	svc, store := fanOutMultiReplicaSvcWithDestroy(t, func(inst *provisionerv1.Instance) error {
		if inst.GetId() == "my-llama-r1" {
			return fmt.Errorf("provider refused")
		}
		destroyed = append(destroyed, inst.GetId())
		return nil
	})
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 3)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	_, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"})
	if err == nil {
		t.Fatal("partial teardown reported success; the operator would stop looking for the leak")
	}
	if !strings.Contains(err.Error(), "my-llama-r1") {
		t.Errorf("error %q should name the replica that failed", err)
	}

	// The other two were still destroyed rather than abandoned at the failure.
	if len(destroyed) != 2 {
		t.Errorf("destroyed %v, want the two healthy replicas", destroyed)
	}
	states := instanceStates(t, store, "my-llama-r0", "my-llama-r2")
	for id, got := range states {
		if got != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
			t.Errorf("%s = %s, want TERMINATED even though a sibling failed", id, got)
		}
	}

	st, _ := store.Read()
	if got := st.Deployments["my-llama"].GetState(); got == provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Error("deployment marked TERMINATED despite a replica surviving")
	}
}

// Retrying a partially-failed teardown converges: replicas already gone are
// skipped, and the deployment reaches TERMINATED once the last one dies.
func TestDestroyDeploymentRetryConverges(t *testing.T) {
	fail := true
	svc, store := fanOutMultiReplicaSvcWithDestroy(t, func(inst *provisionerv1.Instance) error {
		if fail && inst.GetId() == "my-llama-r1" {
			return fmt.Errorf("transient")
		}
		return nil
	})
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 3)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"}); err == nil {
		t.Fatal("first destroy should have failed")
	}

	fail = false
	if _, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"}); err != nil {
		t.Fatalf("retry: %v", err)
	}

	for id, got := range instanceStates(t, store, "my-llama-r0", "my-llama-r1", "my-llama-r2") {
		if got != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
			t.Errorf("%s = %s after retry, want TERMINATED", id, got)
		}
	}
	st, _ := store.Read()
	if got := st.Deployments["my-llama"].GetState(); got != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("deployment state after retry = %s, want TERMINATED", got)
	}
}

// The single-replica path must keep behaving exactly as it did. This is the
// Ch 6 1:1 shape every earlier chapter's demo uses.
func TestDestroyDeploymentSingleReplicaUnchanged(t *testing.T) {
	svc, store := fanOutMultiReplicaSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("solo", 1)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "solo"}); err != nil {
		t.Fatalf("DestroyDeployment: %v", err)
	}
	states := instanceStates(t, store, "solo")
	if got := states["solo"]; got != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
		t.Errorf("single-replica instance = %s, want TERMINATED", got)
	}
}

// Destroy stays idempotent: a second call on a TERMINATED deployment is a
// no-op rather than an error or a second round of provider calls.
func TestDestroyDeploymentIsIdempotent(t *testing.T) {
	calls := 0
	svc, _ := fanOutMultiReplicaSvcWithDestroy(t, func(*provisionerv1.Instance) error {
		calls++
		return nil
	})
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 2)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	for range 2 {
		if _, err := svc.DestroyDeployment(context.Background(),
			&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"}); err != nil {
			t.Fatalf("DestroyDeployment: %v", err)
		}
	}
	if calls != 2 {
		t.Errorf("provider Destroy called %d times for 2 replicas across 2 destroys, want 2", calls)
	}
}

func TestOwnsInstance(t *testing.T) {
	tests := []struct {
		deploy, instance string
		want             bool
	}{
		{"my-llama", "my-llama", true},     // 1:1 carve-out
		{"my-llama", "my-llama-r0", true},  // replica
		{"my-llama", "my-llama-r12", true}, // double-digit slot
		{"my-llama", "shared-pod", false},  // explicitly placed
		{"my-llama", "my-llama-rx", false}, // not a slot number
		{"my-llama", "my-llama-r", false},  // empty slot number
		{"my-llama", "my-llama-extra", false},
		{"my-llama", "", false},
	}
	for _, tt := range tests {
		if got := provisioners.OwnsInstanceForTest(tt.deploy, tt.instance); got != tt.want {
			t.Errorf("ownsInstance(%q, %q) = %v, want %v", tt.deploy, tt.instance, got, tt.want)
		}
	}
}
