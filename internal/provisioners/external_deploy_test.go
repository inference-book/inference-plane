package provisioners_test

import (
	"context"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/external"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

func externalSvc(t *testing.T) (*provisioners.Service, *file.Store) {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{external.New()}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
	)
	return svc, store
}

// TestExternal_Create_AttachesToBothEndpoints: a from-scratch two-replica
// external deploy stamps BOTH slots' engine_endpoints in order and reaches
// RUNNING, with no image and no model validation. This is the case the
// live smoke exposed as [”, 9002]; it must be deterministically correct.
func TestExternal_Create_AttachesToBothEndpoints(t *testing.T) {
	svc, store := externalSvc(t)
	resp, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:         "ext",
			Model:      "mock/mock",
			EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{
			{Provider: provisioners.ProviderExternal, EngineEndpoint: "http://127.0.0.1:9001"},
			{Provider: provisioners.ProviderExternal, EngineEndpoint: "http://127.0.0.1:9002"},
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
	eps := dep.GetEngineEndpoints()
	if len(eps) != 2 || eps[0] != "http://127.0.0.1:9001" || eps[1] != "http://127.0.0.1:9002" {
		t.Errorf("engine_endpoints = %v, want [9001 9002] both stamped in order", eps)
	}

	state, _ := store.Read()
	for _, id := range []string{"ext-r0", "ext-r1"} {
		inst := state.Instances[id]
		if inst == nil {
			t.Errorf("instance %s missing from state", id)
			continue
		}
		if inst.GetProvider() != provisioners.ProviderExternal {
			t.Errorf("instance %s provider = %q, want external", id, inst.GetProvider())
		}
	}
}
