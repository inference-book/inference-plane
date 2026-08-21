package provisioners_test

import (
	"context"
	"errors"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// trackingProvider keeps a provider-side registry, so its Describe answering
// "not found" is evidence the instance is gone.
type trackingProvider struct{ *mockProvider }

func (t *trackingProvider) TracksInstances() bool { return true }

// reconcileSvc wires one provider over a state file holding a single ACTIVE
// instance that points at it.
func reconcileSvc(t *testing.T, p provisioners.Provider) (*provisioners.Service, *file.Store) {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{p}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)))
	if err := store.Update(func(f *provisioners.State) error {
		f.Instances["gone"] = &provisionerv1.Instance{
			Id: "gone", Provider: p.Name(), ProviderId: "p-123",
			State: provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return svc, store
}

func describeRemote(t *testing.T, svc *provisioners.Service) (*provisionerv1.Instance, error) {
	t.Helper()
	resp, err := svc.DescribeInstance(context.Background(), &provisionerv1.DescribeInstanceRequest{
		Id: "gone", Source: provisionerv1.Source_SOURCE_REMOTE,
	})
	return resp.GetInstance(), err
}

func stateOf(t *testing.T, store *file.Store) provisionerv1.InstanceState {
	t.Helper()
	f, err := store.Read()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return f.Instances["gone"].GetState()
}

// An instance the provider no longer has is terminated, and the record should
// say so. Three RunPod pods from June sat ACTIVE in a state file at $0.44/hr
// each, all three 404 at the provider, and the listing an operator checks
// after a crash reported them as running (#396).
func TestDescribeRemoteReconcilesAnInstanceTheProviderNoLongerHas(t *testing.T) {
	p := &trackingProvider{&mockProvider{name: "mock", describe: func(context.Context, string) (*provisionerv1.Instance, error) {
		return nil, provisioners.NewProviderError("mock", "describe", provisioners.ErrNotFound, 404)
	}}}
	svc, store := reconcileSvc(t, p)

	inst, err := describeRemote(t, svc)
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
		t.Errorf("reported state = %s, want TERMINATED", inst.GetState())
	}
	if inst.GetTerminatedAt() == nil {
		t.Error("no terminated_at stamped, so nothing records when we learned")
	}
	if got := stateOf(t, store); got != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
		t.Errorf("stored state = %s, want TERMINATED: the listing keeps lying otherwise", got)
	}
}

// local and external return ErrNotFound from every Describe, because neither
// has a provider-side registry to consult. Reading that as "the instance is
// gone" would terminate every record they own.
func TestDescribeRemoteDoesNotReconcileAProviderWithNoRegistry(t *testing.T) {
	p := &mockProvider{name: "mock", describe: func(context.Context, string) (*provisionerv1.Instance, error) {
		return nil, provisioners.NewProviderError("mock", "describe", provisioners.ErrNotFound, 0)
	}}
	svc, store := reconcileSvc(t, p)

	if _, err := describeRemote(t, svc); err == nil {
		t.Error("want an error: a provider that tracks nothing cannot report absence")
	}
	if got := stateOf(t, store); got != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("stored state = %s, want ACTIVE left alone", got)
	}
}

// Anything that is not a definitive not-found leaves the record alone. A
// provider API that is slow, rate-limiting or briefly broken must never be
// read as evidence a machine stopped billing.
func TestDescribeRemoteLeavesTheRecordAloneOnATransientFailure(t *testing.T) {
	p := &trackingProvider{&mockProvider{name: "mock", describe: func(context.Context, string) (*provisionerv1.Instance, error) {
		return nil, provisioners.NewProviderError("mock", "describe", errors.New("502 bad gateway"), 502)
	}}}
	svc, store := reconcileSvc(t, p)

	if _, err := describeRemote(t, svc); err == nil {
		t.Error("want the provider error surfaced")
	}
	if got := stateOf(t, store); got != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("stored state = %s, want ACTIVE: a failed read is not evidence of absence", got)
	}
}
