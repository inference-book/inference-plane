package provisioners_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/sshkeys"
)

// vmProvider is a VM-style provider: it rents machines and does NOT
// implement Deployer, so deployments onto it go through the sshdocker
// fallback and therefore need a real machine to SSH into.
type vmProvider struct {
	mu         sync.Mutex
	spawns     int
	waitCalls  int
	spawnErr   error
	publishSSH bool
}

func (p *vmProvider) Name() string { return "vmfake" }

func (p *vmProvider) Spawn(_ context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.spawns++
	if p.spawnErr != nil {
		return nil, p.spawnErr
	}
	inst := &provisionerv1.Instance{
		Id:         spec.GetId(),
		Provider:   p.Name(),
		ProviderId: "vm-" + spec.GetId(),
		State:      provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
	}
	if p.publishSSH {
		inst.Ssh = &provisionerv1.SshTarget{Host: "1.2.3.4", Port: 22, User: "root"}
	}
	return inst, nil
}

func (p *vmProvider) Describe(_ context.Context, id string) (*provisionerv1.Instance, error) {
	return &provisionerv1.Instance{Id: id, Provider: p.Name(), ProviderId: id}, nil
}
func (p *vmProvider) Terminate(context.Context, string) error { return nil }
func (p *vmProvider) List(context.Context, map[string]string) ([]*provisionerv1.InstanceRef, error) {
	return nil, nil
}

func (p *vmProvider) WaitForSSHReady(_ context.Context, _ string) (*provisionerv1.SshTarget, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.waitCalls++
	return &provisionerv1.SshTarget{Host: "5.6.7.8", Port: 2222, User: "root"}, nil
}

func (p *vmProvider) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.spawns, p.waitCalls
}

func svcWith(t *testing.T, p provisioners.Provider) (*provisioners.Service, *file.Store) {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	return provisioners.New([]provisioners.Provider{p}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
	), store
}

func deployReq(provider string) *provisionerv1.CreateDeploymentRequest {
	return &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: "d1", Image: "img", Model: "m", EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:     provider,
			Requirements: &provisionerv1.ResourceRequirements{MinVramGb: 8},
		}},
		// Synchronous, so the assertions see the finished fan-out rather
		// than a PENDING record and a goroutine still running.
		Wait: true,
	}
}

// The bug: the fan-out wrote a PENDING record and handed it to the SSH
// executor without ever renting a machine, because Spawn's only caller was
// CreateInstance and auto-provisioned deploys do not go through it.
func TestFanOutSpawnsForVMStyleProviders(t *testing.T) {
	p := &vmProvider{}
	svc, store := svcWith(t, p)

	_, _ = svc.CreateDeployment(context.Background(), deployReq("vmfake"))

	spawns, _ := p.counts()
	if spawns != 1 {
		t.Fatalf("spawns = %d, want 1; the fan-out never rented a machine", spawns)
	}

	f, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	inst := f.Instances["d1"]
	if inst.GetProviderId() == "" {
		t.Error("instance has no provider_id; the executor would dial nothing")
	}
	if inst.GetSsh().GetHost() == "" {
		t.Error("instance has no ssh endpoint after provisioning")
	}
}

// Renting is expensive and silent when wrong. An image-native provider
// creates the machine inside Deploy, so a spawn here would rent a second
// one that nothing tracks.
func TestFanOutDoesNotSpawnForImageNativeProviders(t *testing.T) {
	p := &imageNativeProvider{vmProvider: &vmProvider{}}
	svc, _ := svcWith(t, p)

	_, _ = svc.CreateDeployment(context.Background(), deployReq("vmfake"))

	spawns, _ := p.counts()
	if spawns != 0 {
		t.Errorf("spawns = %d, want 0; an image-native provider rents inside Deploy", spawns)
	}
}

// The endpoint is published after the rental, so a provider that can report
// readiness is consulted before the slot is handed on.
func TestFanOutWaitsForSSHWhenSpawnDoesNotPublishIt(t *testing.T) {
	p := &vmProvider{publishSSH: false}
	svc, store := svcWith(t, p)

	_, _ = svc.CreateDeployment(context.Background(), deployReq("vmfake"))

	_, waits := p.counts()
	if waits != 1 {
		t.Errorf("WaitForSSHReady calls = %d, want 1", waits)
	}
	f, _ := store.Read()
	if got := f.Instances["d1"].GetSsh().GetHost(); got != "5.6.7.8" {
		t.Errorf("ssh host = %q, want the waited-for endpoint", got)
	}
}

// An endpoint returned by Spawn still gets verified. Vast's rent response
// carries ssh_host immediately and the port refuses connections for a while
// afterwards, so trusting the address costs a rented machine and a dial
// timeout that reads like a broken host.
//
// This case asserted the opposite when first written, on the assumption that
// a published address meant a usable one. A live run disproved it: the
// deploy failed with "tcp dial i/o timeout" against a machine that was
// merely young.
func TestFanOutVerifiesEvenAnEndpointSpawnReturned(t *testing.T) {
	p := &vmProvider{publishSSH: true}
	svc, store := svcWith(t, p)

	_, _ = svc.CreateDeployment(context.Background(), deployReq("vmfake"))

	if _, waits := p.counts(); waits != 1 {
		t.Errorf("WaitForSSHReady calls = %d, want 1; a published endpoint is not a reachable one", waits)
	}
	f, _ := store.Read()
	if got := f.Instances["d1"].GetSsh().GetHost(); got != "5.6.7.8" {
		t.Errorf("ssh host = %q, want the verified endpoint to win", got)
	}
}

// A slot that could not be rented must fail as a slot rather than being
// passed to the executor to fail more confusingly later.
func TestFanOutSurfacesSpawnFailure(t *testing.T) {
	p := &vmProvider{spawnErr: context.DeadlineExceeded}
	svc, store := svcWith(t, p)

	_, _ = svc.CreateDeployment(context.Background(), deployReq("vmfake"))

	f, _ := store.Read()
	dep := f.Deployments["d1"]
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("deployment state = %v, want FAILED", dep.GetState())
	}
	if !strings.Contains(dep.GetFailureReason(), "provision replica") {
		t.Errorf("failure reason = %q, want it to name the provisioning step", dep.GetFailureReason())
	}
}

// imageNativeProvider is a vmProvider that also satisfies Deployer, which is
// how the Service tells "rents machines" from "runs images".
type imageNativeProvider struct{ *vmProvider }

func (p *imageNativeProvider) Deploy(context.Context, *provisionerv1.Deployment, *provisionerv1.Instance, *sshkeys.KeyPair, func(provisioners.DeployStateUpdate)) error {
	return nil
}

func (p *imageNativeProvider) Destroy(context.Context, *provisionerv1.Deployment, *provisionerv1.Instance, *sshkeys.KeyPair, func(provisioners.DeployStateUpdate)) error {
	return nil
}
