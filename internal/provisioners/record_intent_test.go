package provisioners_test

import (
	"context"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/external"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// CreateDeployment builds its record field by field, so anything left out of
// that literal is a flag the operator set and nothing downstream ever sees.
// The deployers get this record and so does the router.
//
// This is the test that was missing when upstream auth shipped. Its unit tests
// covered the header construction and the deploy-time validation and neither
// touched persistence, so the credential was accepted, defaulted, and then
// dropped, and every forwarded request went out unauthenticated.
func TestRecordCarriesOperatorIntent(t *testing.T) {
	t.Setenv("TEST_RECORD_KEY", "sk-x")

	store, err := file.Open(t.TempDir(), "op")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	svc := provisioners.New(
		[]provisioners.Provider{local.New(), external.New()}, store, "op",
		provisioners.WithModelStore(nil),
	)

	_, err = svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:               "intent",
			Image:            "vllm/vllm-openai:v0.7.0",
			Model:            "mock/mock",
			EngineEntrypoint: []string{"/usr/bin/entrypoint"},
			UpstreamAuth: &provisionerv1.UpstreamAuth{
				Header: "Authorization", ValueEnv: "TEST_RECORD_KEY", ValuePrefix: "Bearer ",
			},
			Parallelism: &provisionerv1.Parallelism{TensorParallelSize: 4},
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:       provisioners.ProviderExternal,
			Replicas:       1,
			EngineEndpoint: "http://127.0.0.1:9999",
		}},
	})
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	got, err := svc.DescribeDeployment(context.Background(),
		&provisionerv1.DescribeDeploymentRequest{Id: "intent"})
	if err != nil {
		t.Fatalf("DescribeDeployment: %v", err)
	}
	dep := got.GetDeployment()

	if dep.GetUpstreamAuth().GetValueEnv() != "TEST_RECORD_KEY" {
		t.Errorf("upstream_auth = %v; the router reads this record, so a dropped credential means every request goes out unauthenticated",
			dep.GetUpstreamAuth())
	}
	if dep.GetParallelism().GetTensorParallelSize() != 4 {
		t.Errorf("parallelism = %v, want the operator's declared split", dep.GetParallelism())
	}
	if ep := dep.GetEngineEntrypoint(); len(ep) != 1 || ep[0] != "/usr/bin/entrypoint" {
		t.Errorf("engine_entrypoint = %v, want the operator's value", ep)
	}
	// The split is also translated into engine arguments.
	var sawTP bool
	for _, a := range dep.GetEngineArgs() {
		if a == "--tensor-parallel-size=4" {
			sawTP = true
		}
	}
	if !sawTP {
		t.Errorf("engine_args = %v, want the translated tensor-parallel flag", dep.GetEngineArgs())
	}
}
