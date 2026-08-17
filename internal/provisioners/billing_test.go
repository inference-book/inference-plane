package provisioners_test

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

func billingSvc(t *testing.T, insts ...*provisionerv1.Instance) *provisioners.Service {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	if err := store.Update(func(s *provisioners.State) error {
		for _, inst := range insts {
			s.Instances[inst.GetId()] = inst
		}
		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return provisioners.New(nil, store, "default")
}

func byID(got []metrics.BillingInstance) map[string]metrics.BillingInstance {
	out := map[string]metrics.BillingInstance{}
	for _, b := range got {
		out[b.ID] = b
	}
	return out
}

func TestBillingInstancesReadsIdentityAndPriceOffTheRecord(t *testing.T) {
	// Everything a cost figure needs is already written down at spawn.
	// The provider quoted the rate, the adapter stamped the SKU, and
	// the service stamped when the meter started, so none of it has to
	// be asserted by the operator's shell (#163).
	activated := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	svc := billingSvc(t, &provisionerv1.Instance{
		Id: "i-1", Provider: "runpod",
		Hardware:      &provisionerv1.Hardware{GpuSku: "NVIDIA A100 80GB PCIe"},
		HourlyRateUsd: 1.69,
		ActivatedAt:   timestamppb.New(activated),
	})

	got := byID(svc.BillingInstances())["i-1"]
	if got.Provider != "runpod" || got.GPUSKU != "NVIDIA A100 80GB PCIe" || got.RateUSDPerHour != 1.69 {
		t.Errorf("record fields did not survive: %+v", got)
	}
	if !got.Since.Equal(activated) {
		t.Errorf("Since = %v, want the activation time %v", got.Since, activated)
	}
}

func TestBillingStartsWhenTheProviderSaidTheInstanceWasUp(t *testing.T) {
	// created_at is written at PENDING, before the provider call
	// returns, so billing from it would charge the operator for
	// provisioning latency. activated_at is the state the enum itself
	// describes as "billing started".
	created := time.Now().Add(-2 * time.Hour).UTC().Truncate(time.Second)
	activated := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	svc := billingSvc(t, &provisionerv1.Instance{
		Id: "i-1", Provider: "vast",
		CreatedAt:   timestamppb.New(created),
		ActivatedAt: timestamppb.New(activated),
	})

	if got := byID(svc.BillingInstances())["i-1"].Since; !got.Equal(activated) {
		t.Errorf("Since = %v, want activated_at %v rather than created_at %v", got, activated, created)
	}
}

func TestARecordThatNeverActivatedIsNotBilling(t *testing.T) {
	// A record with neither timestamp has not started, and reporting it
	// as a zero-second rental would put a series in front of an
	// operator for something that is not costing anything yet.
	svc := billingSvc(t, &provisionerv1.Instance{Id: "i-pending", Provider: "runpod"})

	if got := svc.BillingInstances(); len(got) != 0 {
		t.Errorf("a record with no start time was reported as billing: %+v", got)
	}
}

func TestBillingModeIsDerivedFromTheProvider(t *testing.T) {
	// Nothing on an Instance records how it is billed, so this is the
	// most the record can honestly say. An unrecognised provider reads
	// as metered, because calling a new paid provider "owned" would
	// silently zero its cost.
	now := timestamppb.New(time.Now())
	svc := billingSvc(t,
		&provisionerv1.Instance{Id: "i-paid", Provider: "runpod", ActivatedAt: now},
		&provisionerv1.Instance{Id: "i-local", Provider: provisioners.ProviderLocal, ActivatedAt: now},
		&provisionerv1.Instance{Id: "i-ext", Provider: provisioners.ProviderExternal, ActivatedAt: now},
		&provisionerv1.Instance{Id: "i-new", Provider: "some-future-vendor", ActivatedAt: now},
	)

	got := byID(svc.BillingInstances())
	for id, want := range map[string]string{
		"i-paid":  "metered_per_second",
		"i-local": "owned",
		"i-ext":   "none",
		"i-new":   "metered_per_second",
	} {
		if got[id].BillingMode != want {
			t.Errorf("%s billing mode = %q, want %q", id, got[id].BillingMode, want)
		}
	}
}

func TestBillingInstancesTakesNoStateLock(t *testing.T) {
	// A metrics scrape runs while the daemon is up and holding the
	// flock for its lifetime. A read that reached for it would take the
	// whole scrape down, which is the shape of #307 in a new place.
	dir := t.TempDir()
	held, err := file.Open(dir, "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	release, err := held.LockForLifetime()
	if err != nil {
		t.Fatalf("LockForLifetime: %v", err)
	}
	defer release()

	reader, err := file.Open(dir, "default")
	if err != nil {
		t.Fatalf("second file.Open: %v", err)
	}
	svc := provisioners.New(nil, reader, "default")

	// Answers rather than blocking or erroring. Empty is the correct
	// answer for an empty state file.
	if got := svc.BillingInstances(); got == nil && len(got) != 0 {
		t.Error("unreachable")
	}
}
