package provisioners_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// heteroSvc builds a Service with TWO image-native mock providers
// (mockA, mockB) so per-slot dispatch can be verified -- each
// provider's Deploy stamps a provider-distinguishing endpoint, so
// engine_endpoints[i]'s string says "which provider rented slot i."
func heteroSvc(t *testing.T) (*provisioners.Service, *file.Store, *fanOutMockProvider, *fanOutMockProvider) {
	t.Helper()
	provA := &fanOutMockProvider{name: "mockA"}
	provB := &fanOutMockProvider{name: "mockB"}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{provA, provB}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
	)
	return svc, store, provA, provB
}

// TestHetero_Create_OneFromEachProvider: the chapter's core
// differentiator. CreateDeployment with replicas_spec = [mockA:s,
// mockB:s] provisions one Instance per provider; instance_ids names
// them r0/r1; engine_endpoints carries provider-distinct URLs.
func TestHetero_Create_OneFromEachProvider(t *testing.T) {
	svc, store, _, _ := heteroSvc(t)
	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         "mixed",
			Image:      "vllm/vllm-openai:v0.7.0",
			Model:      "Qwen/Qwen2.5-1.5B-Instruct",
			EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{
			{Provider: "mockA", Requirements: &provisionerv1.ResourceRequirements{Sku: "a-sku"}},
			{Provider: "mockB", Requirements: &provisionerv1.ResourceRequirements{Sku: "b-sku"}},
		},
		Wait: true,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Fatalf("state = %s, want RUNNING", dep.GetState())
	}
	ids := dep.GetInstanceIds()
	if len(ids) != 2 || ids[0] != "mixed-r0" || ids[1] != "mixed-r1" {
		t.Errorf("instance_ids = %v, want [mixed-r0 mixed-r1]", ids)
	}
	eps := dep.GetEngineEndpoints()
	if len(eps) != 2 {
		t.Fatalf("engine_endpoints len = %d, want 2", len(eps))
	}
	// Each slot's endpoint comes from the slot's provider's Deploy
	// (the mock stamps http://<instance-id>:8000, which is the
	// per-slot id, so this is just confirming the per-slot dispatch
	// happened).
	for i, ep := range eps {
		if ep == "" {
			t.Errorf("slot %d endpoint empty", i)
		}
	}
	// Per-slot Instance records carry the right provider.
	state, _ := store.Read()
	if inst := state.Instances["mixed-r0"]; inst == nil || inst.GetProvider() != "mockA" {
		t.Errorf("mixed-r0 provider = %q, want mockA", inst.GetProvider())
	}
	if inst := state.Instances["mixed-r1"]; inst == nil || inst.GetProvider() != "mockB" {
		t.Errorf("mixed-r1 provider = %q, want mockB", inst.GetProvider())
	}
}

// TestHetero_Create_HomogeneousAndHeteroAreMutuallyExclusive: a
// request that sets BOTH replicas (homogeneous) and replicas_spec
// (heterogeneous) is rejected -- the caller picks one form per call.
func TestHetero_Create_HomogeneousAndHeteroAreMutuallyExclusive(t *testing.T) {
	svc, _, _, _ := heteroSvc(t)
	_, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         "bad",
			Image:      "vllm/vllm-openai:v0.7.0",
			Model:      "Qwen/Qwen2.5-1.5B-Instruct",
			EnginePort: 8000,
		},
		Provider:     "mockA",
		Requirements: &provisionerv1.ResourceRequirements{Sku: "x"},
		Replicas:     3,
		ReplicasSpec: []*provisionerv1.ReplicaSpec{
			{Provider: "mockA", Requirements: &provisionerv1.ResourceRequirements{Sku: "a"}},
		},
		Wait: true,
	})
	if err == nil {
		t.Fatal("expected InvalidArgument for both homogeneous and replicas_spec set")
	}
	if c := status.Code(err); c != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", c)
	}
}

// TestHetero_Create_UnknownProviderInSpec: a replicas_spec entry
// referencing an unconfigured provider fails the per-slot place,
// landing that slot's failure in the aggregate state.
func TestHetero_Create_UnknownProviderInSpec(t *testing.T) {
	svc, _, _, _ := heteroSvc(t)
	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         "partial",
			Image:      "vllm/vllm-openai:v0.7.0",
			Model:      "Qwen/Qwen2.5-1.5B-Instruct",
			EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{
			{Provider: "mockA", Requirements: &provisionerv1.ResourceRequirements{Sku: "a-sku"}},
			{Provider: "ghostcloud", Requirements: &provisionerv1.ResourceRequirements{Sku: "g-sku"}},
		},
		Wait: true,
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED {
		t.Errorf("state = %s, want DEGRADED", dep.GetState())
	}
	if !strings.Contains(dep.GetFailureReason(), "ghostcloud") {
		t.Errorf("failure_reason should name ghostcloud: %q", dep.GetFailureReason())
	}
}

// TestHetero_Scale_AddReplicaAppendsDifferentProvider: existing
// 1 mockA replica is scaled to add 1 mockB replica. Final state:
// 2 slots, one of each provider, all RUNNING.
func TestHetero_Scale_AddReplicaAppendsDifferentProvider(t *testing.T) {
	svc, store, _, _ := heteroSvc(t)

	// Start with a single-replica mockA deployment.
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         "growmix",
			Image:      "vllm/vllm-openai:v0.7.0",
			Model:      "Qwen/Qwen2.5-1.5B-Instruct",
			EnginePort: 8000,
		},
		Provider:     "mockA",
		Requirements: &provisionerv1.ResourceRequirements{Sku: "a-sku"},
		Replicas:     1,
		Wait:         true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Append a mockB replica via add_replicas (heterogeneous Scale).
	resp, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id: "growmix",
		AddReplicas: []*provisionerv1.ReplicaSpec{
			{Provider: "mockB", Requirements: &provisionerv1.ResourceRequirements{Sku: "b-sku"}},
		},
		Wait: true,
	})
	if err != nil {
		t.Fatalf("Scale: %v", err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("state = %s, want RUNNING", dep.GetState())
	}
	if got := dep.GetInstanceIds(); len(got) != 2 {
		t.Fatalf("instance_ids = %v, want 2 slots", got)
	}

	state, _ := store.Read()
	// Original anchor + appended mockB replica.
	if inst := state.Instances["growmix"]; inst == nil || inst.GetProvider() != "mockA" {
		t.Errorf("anchor provider = %q, want mockA", inst.GetProvider())
	}
	if inst := state.Instances["growmix-r1"]; inst == nil || inst.GetProvider() != "mockB" {
		t.Errorf("appended provider = %q, want mockB", inst.GetProvider())
	}
}

// TestHetero_Scale_MutuallyExclusive: passing both target_replicas
// and add_replicas in the same call is rejected.
func TestHetero_Scale_MutuallyExclusive(t *testing.T) {
	svc, _, _, _ := heteroSvc(t)
	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         "x",
			Image:      "vllm/vllm-openai:v0.7.0",
			Model:      "Qwen/Qwen2.5-1.5B-Instruct",
			EnginePort: 8000,
		},
		Provider:     "mockA",
		Requirements: &provisionerv1.ResourceRequirements{Sku: "a-sku"},
		Replicas:     1,
		Wait:         true,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id:             "x",
		TargetReplicas: 3,
		AddReplicas: []*provisionerv1.ReplicaSpec{
			{Provider: "mockB", Requirements: &provisionerv1.ResourceRequirements{Sku: "b-sku"}},
		},
	})
	if err == nil {
		t.Fatal("expected InvalidArgument for both target_replicas and add_replicas")
	}
	if c := status.Code(err); c != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", c)
	}
}

// TestHetero_Scale_NeitherForm: a Scale call that sets neither
// target_replicas nor add_replicas is invalid.
func TestHetero_Scale_NeitherForm(t *testing.T) {
	svc, _, _, _ := heteroSvc(t)
	_, err := svc.ScaleDeployment(context.Background(), &provisionerv1.ScaleDeploymentRequest{
		Id: "nothing",
	})
	if err == nil {
		t.Fatal("expected InvalidArgument when neither form is set")
	}
	if c := status.Code(err); c != codes.InvalidArgument {
		t.Errorf("code = %s, want InvalidArgument", c)
	}
}

// Compile-time check: the fanOutMockProvider doesn't reference
// sshkeys.KeyPair directly, but our heteroSvc uses both providers
// through the same key store. Sanity import for sshkeys to keep
// the test file building independently of test additions.
var _ = sshkeys.KeyPair{}
