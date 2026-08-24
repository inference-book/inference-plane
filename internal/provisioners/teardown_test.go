package provisioners_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
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

// vmStyleSvc builds a Service whose only provider rents machines and does
// not implement Deployer, so deployments onto it go through the configured
// executor the way Lambda Labs does. Returns the provider and the executor
// so a test can count what each of them was asked to do.
func vmStyleSvc(t *testing.T, termFn func(providerID string) error) (*provisioners.Service, *file.Store, *mockProvider, *recordingExecutor) {
	t.Helper()
	prov := &mockProvider{
		name: "vmstyle",
		spawn: func(_ context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
			return &provisionerv1.Instance{
				Id:         spec.GetId(),
				Provider:   "vmstyle",
				ProviderId: "vm-" + spec.GetId(),
				Spec:       spec,
				State:      provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
				Ssh:        &provisionerv1.SshTarget{Host: "1.2.3.4", Port: 22, User: "ubuntu"},
			}, nil
		},
	}
	if termFn != nil {
		prov.term = func(_ context.Context, providerID string) error { return termFn(providerID) }
	}
	exec := &recordingExecutor{}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithDeploymentExecutor(exec))
	return svc, store, prov, exec
}

func vmStyleCreateReq(depID string, replicas int32) *provisionerv1.CreateDeploymentRequest {
	return &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         depID,
			Image:      "vllm/vllm-openai:v0.7.0",
			Model:      "Qwen/Qwen2.5-0.5B-Instruct",
			EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:     "vmstyle",
			Requirements: &provisionerv1.ResourceRequirements{Sku: "mock-sku"},
			Replicas:     replicas,
		}},
		Wait: true,
	}
}

// Issue 161. On a VM-style provider the executor's Destroy stops the engine
// container over SSH and the machine underneath it keeps running, so a
// teardown that only patched the state file left the operator's meter
// running against a deployment they had been told was gone.
func TestDestroyDeploymentReleasesTheRentalOnAVMProvider(t *testing.T) {
	svc, store, prov, exec := vmStyleSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), vmStyleCreateReq("my-llama", 1)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"}); err != nil {
		t.Fatalf("DestroyDeployment: %v", err)
	}
	if exec.destroyCalls != 1 {
		t.Errorf("executor.Destroy calls = %d, want 1", exec.destroyCalls)
	}
	if prov.termCalls != 1 {
		t.Errorf("provider.Terminate calls = %d, want 1 (the VM is still rented until this fires)", prov.termCalls)
	}
	if got := instanceStates(t, store, "my-llama")["my-llama"]; got != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
		t.Errorf("instance state = %s, want TERMINATED", got)
	}
}

// Every replica's machine is handed back, not just the first. A three-node
// deployment that released one VM is two VMs still billing.
func TestDestroyDeploymentReleasesEveryReplicasRental(t *testing.T) {
	svc, _, prov, _ := vmStyleSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), vmStyleCreateReq("my-llama", 3)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"}); err != nil {
		t.Fatalf("DestroyDeployment: %v", err)
	}
	if prov.termCalls != 3 {
		t.Errorf("provider.Terminate calls = %d, want 3 (one per replica)", prov.termCalls)
	}
}

// An image-native provider gets the same call. Its own Destroy already
// released the pod, so this is the second terminate the Provider contract
// promises is a no-op, and keeping the path uniform means a future adapter
// that quietly does not release its machine leaks nothing.
func TestDestroyDeploymentReleasesTheRentalOnAnImageNativeProvider(t *testing.T) {
	prov := &fanOutMockProvider{name: "mockfan"}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)))
	if _, err := svc.CreateDeployment(context.Background(), multiReplicaCreateReq("my-llama", 2)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"}); err != nil {
		t.Fatalf("DestroyDeployment: %v", err)
	}
	// For an image-native provider the pod is the instance, so finalize
	// overwrites provider_id with the container the deploy reported. That
	// is the id the second terminate has to name.
	want := []string{"my-llama-r0-container", "my-llama-r1-container"}
	if !slices.Equal(prov.terminated, want) {
		t.Errorf("terminated = %v, want %v", prov.terminated, want)
	}
}

// An instance the operator placed by hand may be shared, and it is theirs
// to destroy. The same ownership guard that stops the state patch stops the
// provider call, so tearing down a deployment pointed at a long-lived box
// does not take the box with it.
func TestDestroyDeploymentLeavesAnOperatorPlacedRentalAlone(t *testing.T) {
	svc, store, prov, _ := vmStyleSvc(t, nil)
	_ = store.Update(func(f *provisioners.State) error {
		f.Instances["shared-box"] = &provisionerv1.Instance{
			Id: "shared-box", Provider: "vmstyle", ProviderId: "vm-shared-box",
			State:    provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			Hardware: &provisionerv1.Hardware{GpuSku: "mock-sku"},
			Ssh:      &provisionerv1.SshTarget{Host: "1.2.3.4", Port: 22, User: "ubuntu"},
		}
		return nil
	})
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: "my-llama", InstanceId: "shared-box",
			Image: "vllm/vllm-openai:v0.7.0", Model: "Qwen/Qwen2.5-0.5B-Instruct", EnginePort: 8000,
		},
		Wait: true,
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if _, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"}); err != nil {
		t.Fatalf("DestroyDeployment: %v", err)
	}
	if prov.termCalls != 0 {
		t.Errorf("provider.Terminate calls = %d, want 0 for an operator-placed instance", prov.termCalls)
	}
	if got := instanceStates(t, store, "shared-box")["shared-box"]; got != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("shared instance state = %s, want ACTIVE", got)
	}
}

// A machine that would not go away must not read as a clean teardown. The
// deployment stays TERMINATING with the reason attached, which is the handle
// the operator retries on and the signal the reaper picks up.
func TestDestroyDeploymentStaysTerminatingWhenTheRentalSurvives(t *testing.T) {
	svc, store, _, _ := vmStyleSvc(t, func(string) error { return errors.New("provider said no") })
	if _, err := svc.CreateDeployment(context.Background(), vmStyleCreateReq("my-llama", 1)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	_, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"})
	if err == nil {
		t.Fatal("DestroyDeployment reported success while the machine was still rented")
	}
	if !strings.Contains(err.Error(), "provider said no") {
		t.Errorf("error = %v, want it to carry the provider's reason", err)
	}
	st, _ := store.Read()
	if got := st.Deployments["my-llama"].GetState(); got != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING {
		t.Errorf("deployment state = %s, want TERMINATING", got)
	}
	if got := instanceStates(t, store, "my-llama")["my-llama"]; got == provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
		t.Error("instance marked TERMINATED while its machine is still rented")
	}
}
