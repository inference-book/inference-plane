package provisioners_test

import (
	"context"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// silentDeployer is an image-native provider that has its own Deploy and
// never reads Deployment.mounts. Vast was exactly this until #254: it
// accepted a warm-cache mount, ignored it, downloaded the model, and the
// deploy was still labelled storage_tier=warm.
type silentDeployer struct {
	provisioners.Provider
	name string
}

func (s *silentDeployer) Name() string { return s.name }

func (s *silentDeployer) Deploy(_ context.Context, _ *provisionerv1.Deployment, _ *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	emit(provisioners.DeployStateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING})
	return nil
}

func (s *silentDeployer) Destroy(_ context.Context, _ *provisionerv1.Deployment, _ *provisionerv1.Instance, _ *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	emit(provisioners.DeployStateUpdate{State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED})
	return nil
}

// attachingDeployer is the same shape with the capability declared.
type attachingDeployer struct{ silentDeployer }

func (a *attachingDeployer) AttachesMounts() bool { return true }

func mountSvc(t *testing.T, p provisioners.Provider) *provisioners.Service {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	return provisioners.New([]provisioners.Provider{p}, store, "default")
}

func mountedDeployReq(id, provider string) *provisionerv1.CreateDeploymentRequest {
	return &provisionerv1.CreateDeploymentRequest{
		Wait: true,
		Deployment: &provisionerv1.Deployment{
			Id: id, Image: "vllm/vllm-openai:v0.7.0", Model: "Qwen/Qwen2.5-32B", EnginePort: 8000,
			Mounts: []*provisionerv1.VolumeMount{{
				VolumeId: "vol-1", MountPath: "/models", Provider: provider,
			}},
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider: provider, Replicas: 1,
			Requirements: &provisionerv1.ResourceRequirements{Sku: "mock-sku"},
		}},
	}
}

func TestADeployPathThatIgnoresMountsRefusesThem(t *testing.T) {
	// The silent drop is the bug. An operator who configured a warm
	// cache and got a cold download would see nothing wrong until the
	// bill, and the deploy would report storage_tier=warm the whole
	// time, because that label is derived from the mount being asked
	// for rather than from it being attached (#254).
	svc := mountSvc(t, &silentDeployer{Provider: local.New(), name: "silent"})

	_, err := svc.CreateDeployment(context.Background(), mountedDeployReq("d1", "silent"))
	if err == nil {
		t.Fatal("a provider that cannot attach a mount accepted one")
	}
	// The refusal has to say what to do. Both routes out are real
	// choices an operator might want.
	for _, want := range []string{"cannot attach", "model_cache", "cold"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
}

func TestDeclaringTheCapabilityIsWhatLetsTheMountThrough(t *testing.T) {
	// Same provider, same request, one method added. Pins that the
	// declaration is load-bearing rather than decorative, so a future
	// change that stops consulting it fails here.
	svc := mountSvc(t, &attachingDeployer{silentDeployer{Provider: local.New(), name: "attaching"}})

	if _, err := svc.CreateDeployment(context.Background(), mountedDeployReq("d2", "attaching")); err != nil {
		t.Errorf("a provider that declares it attaches mounts was refused: %v", err)
	}
}

func TestADeployWithNoMountsIsUnaffected(t *testing.T) {
	// The overwhelming majority of deploys. A guard that made cold
	// deploys harder would be a worse bug than the one it fixes.
	svc := mountSvc(t, &silentDeployer{Provider: local.New(), name: "silent"})

	req := mountedDeployReq("d3", "silent")
	req.Deployment.Mounts = nil

	if _, err := svc.CreateDeployment(context.Background(), req); err != nil {
		t.Errorf("a deploy with no mounts was refused: %v", err)
	}
}

func TestAProviderWithoutItsOwnDeployerStillAttaches(t *testing.T) {
	// VM-style providers route through the sshdocker executor, which
	// binds host paths. They have no Deployer to declare anything on,
	// and fail-closed must not sweep them up.
	svc := mountSvc(t, local.New())

	req := mountedDeployReq("d4", local.New().Name())
	// A host-path bind rather than a provider volume, which is what
	// that path actually honours.
	req.Deployment.Mounts = []*provisionerv1.VolumeMount{{HostPath: "/srv/models", MountPath: "/models"}}
	req.ReplicasSpec[0].Provider = local.New().Name()

	_, err := svc.CreateDeployment(context.Background(), req)
	if err != nil && strings.Contains(err.Error(), "cannot attach") {
		t.Errorf("the sshdocker fallback path was refused a mount it can bind: %v", err)
	}
}
