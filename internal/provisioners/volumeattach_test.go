package provisioners_test

import (
	"context"
	"slices"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// specRecordingProvider is a VM-style provider that keeps the Spec it was
// asked to fulfil, so a test can assert what provisioning was told.
type specRecordingProvider struct {
	*mockProvider
	specs []*provisionerv1.Spec
}

func (p *specRecordingProvider) Spawn(_ context.Context, spec *provisionerv1.Spec) (*provisionerv1.Instance, error) {
	p.specs = append(p.specs, spec)
	return &provisionerv1.Instance{
		Id: spec.GetId(), Provider: p.Name(), ProviderId: "vm-" + spec.GetId(),
		Spec: spec, State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		Ssh: &provisionerv1.SshTarget{Host: "1.2.3.4", Port: 22, User: "ubuntu"},
	}, nil
}

func (p *specRecordingProvider) lastSpec(t *testing.T) *provisionerv1.Spec {
	t.Helper()
	if len(p.specs) == 0 {
		t.Fatal("provider was never asked to spawn anything")
	}
	return p.specs[len(p.specs)-1]
}

// pinnedVolumeSvc builds a Service whose pin registry already holds a warm
// volume for (provider, region) with model staged on it, which is the state
// `iplane model pin` leaves behind.
func pinnedVolumeSvc(t *testing.T, vol *provisionerv1.Volume) (*provisioners.Service, *specRecordingProvider) {
	t.Helper()
	prov := &specRecordingProvider{mockProvider: &mockProvider{name: "vmstyle"}}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	if err := store.Update(func(f *provisioners.State) error {
		if f.Volumes == nil {
			f.Volumes = map[string]*provisionerv1.Volume{}
		}
		f.Volumes[vol.GetId()] = vol
		return nil
	}); err != nil {
		t.Fatalf("seed volume: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithDeploymentExecutor(&recordingExecutor{}))
	return svc, prov
}

func pinnedWarmDeployReq(provider string) *provisionerv1.CreateDeploymentRequest {
	return &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: "my-llama", Image: "vllm/vllm-openai:v0.7.0",
			Model: "Qwen/Qwen2.5-0.5B-Instruct", EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:     provider,
			Region:       "us-east-1",
			Requirements: &provisionerv1.ResourceRequirements{Sku: "mock-sku"},
			Replicas:     1,
		}},
		Wait: true,
	}
}

func lambdaishVolume() *provisionerv1.Volume {
	return &provisionerv1.Volume{
		Id: "iplane-cache-us-east-1", Provider: "vmstyle", Region: "us-east-1",
		Name: "iplane-cache-us-east-1", MountPath: "/models",
		HostPath: "/lambda/nfs/iplane-cache-us-east-1",
		Models:   []string{"Qwen/Qwen2.5-0.5B-Instruct"},
	}
}

// A VM-style provider has to attach the volume while it is renting the
// machine, because the host directory does not exist until it does and the
// executor binds host paths at deploy time. By then it is too late to ask.
func TestProvisioningCarriesTheVolumeHandleForAVMProvider(t *testing.T) {
	svc, prov := pinnedVolumeSvc(t, lambdaishVolume())
	if _, err := svc.CreateDeployment(context.Background(), pinnedWarmDeployReq("vmstyle")); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	got := prov.lastSpec(t).GetVolumeIds()
	if !slices.Contains(got, "iplane-cache-us-east-1") {
		t.Errorf("Spec.volume_ids = %v, want the pinned volume; the filesystem is never attached without it", got)
	}
}

// The host path is recorded by the adapter at EnsureVolume time and carried
// through, so the shared path never learns any provider's directory layout.
// It is also what the sshdocker executor binds.
func TestWarmMountCarriesTheHostPath(t *testing.T) {
	svc, _ := pinnedVolumeSvc(t, lambdaishVolume())
	resp, err := svc.CreateDeployment(context.Background(), pinnedWarmDeployReq("vmstyle"))
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	mounts := resp.GetDeployment().GetMounts()
	if len(mounts) != 1 {
		t.Fatalf("got %d mounts, want 1 resolved from the pin registry", len(mounts))
	}
	if got := mounts[0].GetHostPath(); got != "/lambda/nfs/iplane-cache-us-east-1" {
		t.Errorf("mount host_path = %q, want the path the adapter recorded", got)
	}
	if got := mounts[0].GetVolumeId(); got != "iplane-cache-us-east-1" {
		t.Errorf("mount volume_id = %q", got)
	}
}

// A volume handle only means something to the provider that issued it, and
// the deployment guard already refuses a cross-provider mount. Provisioning
// must not hand one over either.
func TestProvisioningOmitsAVolumeFromAnotherProvider(t *testing.T) {
	vol := lambdaishVolume()
	vol.Provider = "someone-else"
	svc, prov := pinnedVolumeSvc(t, vol)
	if _, err := svc.CreateDeployment(context.Background(), pinnedWarmDeployReq("vmstyle")); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got := prov.lastSpec(t).GetVolumeIds(); len(got) != 0 {
		t.Errorf("Spec.volume_ids = %v, want none; that handle belongs to another provider", got)
	}
}

// A cold deploy stays cold. Nothing pinned means nothing attached, rather
// than an empty handle the adapter has to defend against.
func TestProvisioningCarriesNoVolumeWhenNothingIsPinned(t *testing.T) {
	vol := lambdaishVolume()
	vol.Models = nil
	svc, prov := pinnedVolumeSvc(t, vol)
	if _, err := svc.CreateDeployment(context.Background(), pinnedWarmDeployReq("vmstyle")); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got := prov.lastSpec(t).GetVolumeIds(); len(got) != 0 {
		t.Errorf("Spec.volume_ids = %v, want none for a cold deploy", got)
	}
}
