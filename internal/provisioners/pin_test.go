package provisioners_test

import (
	"context"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// fakeVolumeProvider is a Provider (via the embedded mockProvider) that
// also implements VolumeManager, recording calls so the pin tests can
// assert what the Service drove. EnsureVolume returns a stable id per
// (name, region) so repeated pins accumulate on one volume.
type fakeVolumeProvider struct {
	*mockProvider
	ensured   map[string]provisioners.VolumeRef
	staged    []string
	deleted   []string
	failStage bool
}

func newFakeVolumeProvider(name string) *fakeVolumeProvider {
	return &fakeVolumeProvider{
		mockProvider: &mockProvider{name: name},
		ensured:      map[string]provisioners.VolumeRef{},
	}
}

func (p *fakeVolumeProvider) EnsureVolume(_ context.Context, spec provisioners.VolumeSpec) (provisioners.VolumeRef, error) {
	key := spec.Name + "|" + spec.Region
	if v, ok := p.ensured[key]; ok {
		return v, nil
	}
	ref := provisioners.VolumeRef{ID: "vol-" + spec.Region, Name: spec.Name, Region: spec.Region, SizeGB: spec.SizeGB}
	p.ensured[key] = ref
	return ref, nil
}

func (p *fakeVolumeProvider) StageModel(_ context.Context, spec provisioners.StageSpec) error {
	if p.failStage {
		return context.DeadlineExceeded
	}
	p.staged = append(p.staged, spec.Model)
	return nil
}

func (p *fakeVolumeProvider) ListVolumes(context.Context) ([]provisioners.VolumeRef, error) {
	out := make([]provisioners.VolumeRef, 0, len(p.ensured))
	for _, v := range p.ensured {
		out = append(out, v)
	}
	return out, nil
}

func (p *fakeVolumeProvider) DeleteVolume(_ context.Context, id string) error {
	p.deleted = append(p.deleted, id)
	return nil
}

func newPinService(t *testing.T, prov provisioners.Provider) *provisioners.Service {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	return provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)))
}

func TestPinModel_StagesAndRecordsInRegistry(t *testing.T) {
	prov := newFakeVolumeProvider("runpod")
	svc := newPinService(t, prov)

	res, err := svc.PinModel(context.Background(), provisioners.PinModelRequest{
		Model: "Qwen/Qwen2.5-32B-Instruct-AWQ", Provider: "runpod", Region: "EU-RO-1",
	})
	if err != nil {
		t.Fatalf("PinModel: %v", err)
	}
	if res.AlreadyStaged {
		t.Error("first pin should stage, not report already-staged")
	}
	if len(prov.staged) != 1 {
		t.Errorf("StageModel called %d times, want 1", len(prov.staged))
	}
	v := res.Volume
	if v.GetId() != "vol-EU-RO-1" || v.GetProvider() != "runpod" || v.GetMountPath() != provisioners.DefaultCacheMountPath {
		t.Errorf("volume = %+v, want vol-EU-RO-1/runpod/%s", v, provisioners.DefaultCacheMountPath)
	}
	if len(v.GetModels()) != 1 || v.GetModels()[0] != "Qwen/Qwen2.5-32B-Instruct-AWQ" {
		t.Errorf("models = %v, want the pinned model", v.GetModels())
	}
	// Persisted to the registry.
	vols, _ := svc.ListVolumes(context.Background(), "")
	if len(vols) != 1 {
		t.Fatalf("registry has %d volumes, want 1", len(vols))
	}
}

func TestPinModel_SecondModelAccumulatesOnSameVolume(t *testing.T) {
	prov := newFakeVolumeProvider("runpod")
	svc := newPinService(t, prov)
	region := "EU-RO-1"
	_, _ = svc.PinModel(context.Background(), provisioners.PinModelRequest{Model: "modelA", Provider: "runpod", Region: region})
	res, err := svc.PinModel(context.Background(), provisioners.PinModelRequest{Model: "modelB", Provider: "runpod", Region: region})
	if err != nil {
		t.Fatalf("PinModel B: %v", err)
	}
	if res.Volume.GetId() != "vol-EU-RO-1" {
		t.Errorf("second pin created a new volume %q, want the shared vol-EU-RO-1", res.Volume.GetId())
	}
	if len(res.Volume.GetModels()) != 2 {
		t.Errorf("models = %v, want both accumulated on one volume", res.Volume.GetModels())
	}
}

func TestPinModel_AlreadyStagedSkipsRestage(t *testing.T) {
	prov := newFakeVolumeProvider("runpod")
	svc := newPinService(t, prov)
	req := provisioners.PinModelRequest{Model: "modelA", Provider: "runpod", Region: "EU-RO-1"}
	_, _ = svc.PinModel(context.Background(), req)
	res, err := svc.PinModel(context.Background(), req)
	if err != nil {
		t.Fatalf("PinModel again: %v", err)
	}
	if !res.AlreadyStaged {
		t.Error("re-pinning the same model should report already-staged")
	}
	if len(prov.staged) != 1 {
		t.Errorf("StageModel called %d times, want 1 (no re-stage)", len(prov.staged))
	}
}

func TestPinModel_ForceRestages(t *testing.T) {
	prov := newFakeVolumeProvider("runpod")
	svc := newPinService(t, prov)
	req := provisioners.PinModelRequest{Model: "modelA", Provider: "runpod", Region: "EU-RO-1"}
	_, _ = svc.PinModel(context.Background(), req)
	req.Force = true
	_, err := svc.PinModel(context.Background(), req)
	if err != nil {
		t.Fatalf("PinModel force: %v", err)
	}
	if len(prov.staged) != 2 {
		t.Errorf("StageModel called %d times, want 2 (force re-stages)", len(prov.staged))
	}
}

func TestPinModel_UnsupportedProviderIsUnimplemented(t *testing.T) {
	// Plain mockProvider does not implement VolumeManager.
	svc := newPinService(t, &mockProvider{name: "local"})
	_, err := svc.PinModel(context.Background(), provisioners.PinModelRequest{Model: "m", Provider: "local", Region: "x"})
	if err == nil {
		t.Fatal("pinning on a provider without volumes should fail")
	}
}

func TestUnpinModel_SingleModelKeepsVolume(t *testing.T) {
	prov := newFakeVolumeProvider("runpod")
	svc := newPinService(t, prov)
	region := "EU-RO-1"
	_, _ = svc.PinModel(context.Background(), provisioners.PinModelRequest{Model: "modelA", Provider: "runpod", Region: region})
	_, _ = svc.PinModel(context.Background(), provisioners.PinModelRequest{Model: "modelB", Provider: "runpod", Region: region})

	v, err := svc.UnpinModel(context.Background(), provisioners.UnpinRequest{VolumeID: "vol-EU-RO-1", Model: "modelA"})
	if err != nil {
		t.Fatalf("UnpinModel: %v", err)
	}
	if v == nil || len(v.GetModels()) != 1 || v.GetModels()[0] != "modelB" {
		t.Errorf("after unpinning modelA, models = %v, want [modelB]", v.GetModels())
	}
	if len(prov.deleted) != 0 {
		t.Error("single-model unpin must not destroy the volume")
	}
}

func TestUnpinModel_WholeVolumeDestroys(t *testing.T) {
	prov := newFakeVolumeProvider("runpod")
	svc := newPinService(t, prov)
	_, _ = svc.PinModel(context.Background(), provisioners.PinModelRequest{Model: "modelA", Provider: "runpod", Region: "EU-RO-1"})

	v, err := svc.UnpinModel(context.Background(), provisioners.UnpinRequest{VolumeID: "vol-EU-RO-1"})
	if err != nil {
		t.Fatalf("UnpinModel: %v", err)
	}
	if v != nil {
		t.Errorf("whole-volume unpin should return nil, got %+v", v)
	}
	if len(prov.deleted) != 1 || prov.deleted[0] != "vol-EU-RO-1" {
		t.Errorf("provider DeleteVolume calls = %v, want [vol-EU-RO-1]", prov.deleted)
	}
	vols, _ := svc.ListVolumes(context.Background(), "")
	if len(vols) != 0 {
		t.Errorf("registry still has %d volumes after destroy, want 0", len(vols))
	}
}
