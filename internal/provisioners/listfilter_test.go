package provisioners_test

import (
	"context"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// listRecordingProvider answers List from a fixed account and records the
// filter it was handed, so a test can assert what the Service asked for as
// well as what it did with the answer.
type listRecordingProvider struct {
	*mockProvider
	account     []*provisionerv1.InstanceRef
	seenFilters []map[string]string
	// strict mirrors an adapter that applies match-all over the tags it
	// recovered; lenient mirrors one that drops the filter entirely. Both
	// shapes shipped, on different providers, at the same time.
	strict bool
}

func (p *listRecordingProvider) List(_ context.Context, filter map[string]string) ([]*provisionerv1.InstanceRef, error) {
	p.seenFilters = append(p.seenFilters, filter)
	if !p.strict {
		return p.account, nil
	}
	return provisioners.FilterRefs(p.account, filter), nil
}

func svcWithAccount(t *testing.T, strict bool, account ...*provisionerv1.InstanceRef) (*provisioners.Service, *file.Store, *listRecordingProvider) {
	t.Helper()
	prov := &listRecordingProvider{
		mockProvider: &mockProvider{name: "mock"},
		account:      account,
		strict:       strict,
	}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	return provisioners.New([]provisioners.Provider{prov}, store, "default"), store, prov
}

func seedTerminating(t *testing.T, store *file.Store, id string) {
	t.Helper()
	if err := store.Update(func(f *provisioners.State) error {
		f.Instances[id] = &provisionerv1.Instance{
			Id: id, Provider: "mock", ProviderId: "mock:" + id,
			State:     provisionerv1.InstanceState_INSTANCE_STATE_TERMINATING,
			CreatedAt: timestamppb.Now(),
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func instanceState(t *testing.T, store *file.Store, id string) provisionerv1.InstanceState {
	t.Helper()
	f, err := store.Read()
	if err != nil {
		t.Fatalf("store.Read: %v", err)
	}
	return f.Instances[id].GetState()
}

// The self-heal reads an empty list as "the provider says this instance is
// gone". It has to ask a question that can come back empty for that reason
// and no other, which means filtering on a tag every adapter can recover.
// Asking for iplane-operator alongside it is what broke this: no adapter
// recovers that tag, so a strict one answered empty for every instance
// alive or dead.
func TestListInstancesSelfHealAsksOnlyForTheID(t *testing.T) {
	svc, store, prov := svcWithAccount(t, true)
	seedTerminating(t, store, "my-pod")
	if _, err := svc.ListInstances(context.Background(), &provisionerv1.ListInstancesRequest{}); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(prov.seenFilters) == 0 {
		t.Fatal("provider.List was never called")
	}
	for _, f := range prov.seenFilters {
		if _, asked := f[provisioners.TagOperator]; asked {
			t.Errorf("self-heal filtered on %s, which no adapter recovers: %v", provisioners.TagOperator, f)
		}
		if f[provisioners.TagID] == "" {
			t.Errorf("self-heal filter carries no %s: %v", provisioners.TagID, f)
		}
	}
}

// A pod the provider still has must not be declared gone. On a strict
// adapter the old filter matched nothing whatever the account held, so a
// record stuck in TERMINATING because its terminate errored was quietly
// marked TERMINATED and nothing retried it. The machine went on billing.
func TestListInstancesDoesNotDeclareALiveInstanceTerminated(t *testing.T) {
	svc, store, _ := svcWithAccount(t, true, &provisionerv1.InstanceRef{
		ProviderId:    "mock:my-pod",
		ProviderState: "running",
		Tags:          map[string]string{provisioners.TagID: "my-pod"},
	})
	seedTerminating(t, store, "my-pod")
	if _, err := svc.ListInstances(context.Background(), &provisionerv1.ListInstancesRequest{}); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if got := instanceState(t, store, "my-pod"); got == provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
		t.Error("instance marked TERMINATED while the provider still has it")
	}
}

// The other half, and the direction a lenient adapter failed in. A record
// whose machine really is gone has to converge, and it has to converge while
// the account holds other instances, because that is every account.
func TestListInstancesConvergesATerminatingRecordAlongsideOtherInstances(t *testing.T) {
	svc, store, _ := svcWithAccount(t, false, &provisionerv1.InstanceRef{
		ProviderId:    "mock:someone-else",
		ProviderState: "running",
		Tags:          map[string]string{provisioners.TagID: "someone-else"},
	})
	seedTerminating(t, store, "my-pod")
	if _, err := svc.ListInstances(context.Background(), &provisionerv1.ListInstancesRequest{}); err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if got := instanceState(t, store, "my-pod"); got != provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED {
		t.Errorf("state = %s, want TERMINATED; the provider has no record of this instance", got)
	}
}

// --remote answers "what has iplane got on this account". A box the operator
// rented by hand is not an iplane instance, and rendering it as one invites
// somebody to destroy it through a verb that says iplane owns it.
func TestListInstancesRemoteReturnsOnlyIplaneInstances(t *testing.T) {
	svc, _, _ := svcWithAccount(t, false,
		&provisionerv1.InstanceRef{
			ProviderId: "mock:mine", ProviderState: "running",
			Tags: map[string]string{provisioners.TagID: "mine"},
		},
		&provisionerv1.InstanceRef{
			ProviderId: "mock:theirs", ProviderState: "running",
		},
	)
	resp, err := svc.ListInstances(context.Background(), &provisionerv1.ListInstancesRequest{
		Source:   provisionerv1.Source_SOURCE_REMOTE,
		Provider: "mock",
	})
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(resp.GetInstances()) != 1 {
		t.Fatalf("got %d instances, want 1 (the operator's own box is not ours)", len(resp.GetInstances()))
	}
	if got := resp.GetInstances()[0].GetProviderId(); got != "mock:mine" {
		t.Errorf("returned %q, want mock:mine", got)
	}
}
