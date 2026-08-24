package provisioners_test

import (
	"context"
	"fmt"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// vmDeployProvider rents machines, publishes an SSH endpoint through the
// readiness wait the way Lambda does, and does not implement Deployer, so the
// deploy goes through the configured executor.
type vmDeployProvider struct {
	*mockProvider
	waitCalls int
}

func (p *vmDeployProvider) WaitForSSHReady(_ context.Context, _ string) (*provisionerv1.SshTarget, error) {
	p.waitCalls++
	return &provisionerv1.SshTarget{Host: "10.0.0.5", Port: 22, User: "ubuntu"}, nil
}

func (p *vmDeployProvider) IsActiveProviderState(string) bool { return true }

// sshRequiringExecutor mirrors sshdocker.Executor's own precondition: it
// cannot do anything to a machine it has no address for. Without this the
// double happily "tears down" an instance the real executor would refuse, and
// the test proves nothing.
type sshRequiringExecutor struct{ recordingExecutor }

func (e *sshRequiringExecutor) Deploy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	if inst.GetSsh().GetHost() == "" {
		return fmt.Errorf("instance %q has no SSH endpoint (deployment requires an SSH-reachable instance)", inst.GetId())
	}
	return e.recordingExecutor.Deploy(ctx, dep, inst, key, emit)
}

func (e *sshRequiringExecutor) Destroy(ctx context.Context, dep *provisionerv1.Deployment, inst *provisionerv1.Instance, key *sshkeys.KeyPair, emit func(provisioners.DeployStateUpdate)) error {
	if inst.GetSsh().GetHost() == "" {
		return fmt.Errorf("instance %q has no SSH endpoint (deployment requires an SSH-reachable instance)", inst.GetId())
	}
	return e.recordingExecutor.Destroy(ctx, dep, inst, key, emit)
}

// A VM-style deployment has to stay destroyable after the deploy returns.
//
// The endpoint is the only handle a later teardown has: `deployment destroy`
// SSHes in to stop the container, and on a provider whose Destroy does not
// release the machine, that teardown is also what leads to the rental being
// handed back. finalizeInstanceFromDeploy cleared the field, so a real
// Lambda deployment reached RUNNING, served tokens, and then could not be
// destroyed by any later process: every attempt failed with "instance has no
// SSH endpoint" and the machine billed on (#427).
//
// The clearing was written for image-native providers, where Describe's
// publicIp is unverified and iplane never SSHes. On the VM path the record
// already holds an endpoint that WaitForSSHReady dialled and the executor
// then used, and patchRecord replaces the whole record, so unset means
// erased.
func TestDeployKeepsTheVerifiedSSHEndpointOnTheRecord(t *testing.T) {
	prov := &vmDeployProvider{mockProvider: &mockProvider{
		name: "vmstyle",
		spawn: func(_ context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
			// Spawn returns before the address exists, which is why the
			// readiness wait is what supplies it.
			return &provisionerv1.Instance{
				Id: spec.GetId(), Provider: "vmstyle",
				ProviderId: "vm-" + spec.GetId(), Spec: spec,
				State: provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
			}, nil
		},
		describe: func(_ context.Context, providerID string) (*provisionerv1.Instance, error) {
			// Describe knows nothing about port 22, exactly as the comment
			// on the cleared field says.
			return &provisionerv1.Instance{
				Provider: "vmstyle", ProviderId: providerID,
				State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			}, nil
		},
	}}

	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithDeploymentExecutor(&recordingExecutor{}))

	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: "my-llama", Image: "vllm/vllm-openai:v0.7.0",
			Model: "Qwen/Qwen2.5-0.5B-Instruct", EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:     "vmstyle",
			Requirements: &provisionerv1.ResourceRequirements{Sku: "mock-sku"},
			Replicas:     1,
		}},
		Wait: true,
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if prov.waitCalls == 0 {
		t.Fatal("the readiness wait never ran; this test is not exercising the endpoint it checks")
	}

	f, _ := store.Read()
	got := f.Instances["my-llama"].GetSsh()
	if got.GetHost() != "10.0.0.5" || got.GetPort() != 22 || got.GetUser() != "ubuntu" {
		t.Fatalf("persisted ssh = %+v, want the dialled endpoint; without it the deployment cannot be destroyed", got)
	}
}

// The same deployment must actually tear down afterwards, which is the
// consequence the field exists for rather than a restatement of it.
func TestAVMDeploymentCanStillBeDestroyedAfterwards(t *testing.T) {
	prov := &vmDeployProvider{mockProvider: &mockProvider{
		name: "vmstyle",
		spawn: func(_ context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
			return &provisionerv1.Instance{
				Id: spec.GetId(), Provider: "vmstyle",
				ProviderId: "vm-" + spec.GetId(), Spec: spec,
				State: provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
			}, nil
		},
		describe: func(_ context.Context, providerID string) (*provisionerv1.Instance, error) {
			return &provisionerv1.Instance{
				Provider: "vmstyle", ProviderId: providerID,
				State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
			}, nil
		},
	}}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithDeploymentExecutor(&sshRequiringExecutor{}))

	if _, err := svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: "my-llama", Image: "vllm/vllm-openai:v0.7.0",
			Model: "Qwen/Qwen2.5-0.5B-Instruct", EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:     "vmstyle",
			Requirements: &provisionerv1.ResourceRequirements{Sku: "mock-sku"},
			Replicas:     1,
		}},
		Wait: true,
	}); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}

	if _, err := svc.DestroyDeployment(context.Background(),
		&provisionerv1.DestroyDeploymentRequest{Id: "my-llama"}); err != nil {
		t.Fatalf("DestroyDeployment: %v", err)
	}
	if prov.termCalls != 1 {
		t.Errorf("provider.Terminate calls = %d, want 1; the machine was never released", prov.termCalls)
	}
}
