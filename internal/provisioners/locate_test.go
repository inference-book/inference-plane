package provisioners_test

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"google.golang.org/protobuf/types/known/structpb"
)

// locateSvc builds a Service over a state file seeded with instances and
// deployments, which is all LocateEngineNode reads.
func locateSvc(t *testing.T, seed func(*provisioners.State)) *provisioners.Service {
	t.Helper()
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	if err := store.Update(func(s *provisioners.State) error {
		seed(s)
		return nil
	}); err != nil {
		t.Fatalf("seed state: %v", err)
	}
	return provisioners.New(nil, store, "default")
}

// The RunPod adapter records the machine id as a string under
// "runpod.machine_id"; the endpoint comes from the deployment's slot.
func TestLocateEngineNodeJoinsInstanceAndSlot(t *testing.T) {
	svc := locateSvc(t, func(s *provisioners.State) {
		s.Instances["dep-1-r1"] = &provisionerv1.Instance{
			Id:       "dep-1-r1",
			Provider: "runpod",
			Metadata: map[string]*structpb.Value{
				"runpod.machine_id": structpb.NewStringValue("machine-abc"),
			},
		}
		s.Deployments["dep-1"] = &provisionerv1.Deployment{
			Id:              "dep-1",
			InstanceIds:     []string{"dep-1-r0", "dep-1-r1"},
			EngineEndpoints: []string{"https://r0.example.com", "https://r1.example.com"},
		}
	})

	host, provider, endpoint, found, err := svc.LocateEngineNode("dep-1-r1")
	if err != nil {
		t.Fatalf("LocateEngineNode: %v", err)
	}
	if !found {
		t.Fatal("found = false for a provisioned instance")
	}
	if host != "machine-abc" || provider != "runpod" {
		t.Errorf("host/provider = %q/%q, want machine-abc/runpod", host, provider)
	}
	if endpoint != "https://r1.example.com" {
		t.Errorf("endpoint = %q, want slot 1's", endpoint)
	}
}

// Vast records the machine id as a number. Both shapes are read rather than
// one being declared canonical; the value is opaque to the control plane.
func TestLocateEngineNodeReadsNumericMachineID(t *testing.T) {
	svc := locateSvc(t, func(s *provisioners.State) {
		s.Instances["dep-2"] = &provisionerv1.Instance{
			Id:       "dep-2",
			Provider: "vast",
			Metadata: map[string]*structpb.Value{
				"vast.machine_id": structpb.NewNumberValue(143870),
			},
		}
	})

	host, provider, _, found, err := svc.LocateEngineNode("dep-2")
	if err != nil || !found {
		t.Fatalf("LocateEngineNode: found=%v err=%v", found, err)
	}
	if host != "143870" || provider != "vast" {
		t.Errorf("host/provider = %q/%q, want 143870/vast", host, provider)
	}
}

// An engine that registered without iplane provisioning it is a legitimate
// case, reported as not-found rather than as an error.
func TestLocateEngineNodeUnknownIsNotAnError(t *testing.T) {
	svc := locateSvc(t, func(*provisioners.State) {})

	_, _, _, found, err := svc.LocateEngineNode("someone-elses-engine")
	if err != nil {
		t.Fatalf("unknown engine returned an error: %v", err)
	}
	if found {
		t.Error("found = true for an engine we never provisioned")
	}
}

// A provider that records no machine id yields an empty host, which reads as
// an unattributable node because that is exactly what it is.
func TestLocateEngineNodeWithoutMachineID(t *testing.T) {
	svc := locateSvc(t, func(s *provisioners.State) {
		s.Instances["dep-3"] = &provisionerv1.Instance{Id: "dep-3", Provider: "local"}
	})

	host, provider, _, found, err := svc.LocateEngineNode("dep-3")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if host != "" {
		t.Errorf("host = %q, want empty for a provider that records none", host)
	}
	if provider != "local" {
		t.Errorf("provider = %q, want local", provider)
	}
}

// engine_endpoints is the real per-replica set; the singular is legacy and
// only stamped for the count==1 case, so it is a fallback and never primary.
func TestLocateEngineNodeFallsBackToSingularEndpoint(t *testing.T) {
	svc := locateSvc(t, func(s *provisioners.State) {
		s.Instances["dep-4"] = &provisionerv1.Instance{Id: "dep-4", Provider: "runpod"}
		s.Deployments["dep-4"] = &provisionerv1.Deployment{
			Id:             "dep-4",
			InstanceIds:    []string{"dep-4"},
			EngineEndpoint: "https://legacy-singular.example.com",
		}
	})

	_, _, endpoint, found, err := svc.LocateEngineNode("dep-4")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if endpoint != "https://legacy-singular.example.com" {
		t.Errorf("endpoint = %q, want the singular fallback", endpoint)
	}
}

// An instance with no deployment pointing at it is still a located node: the
// host id is real even when nothing is deployed onto it yet.
func TestLocateEngineNodeInstanceWithoutDeployment(t *testing.T) {
	svc := locateSvc(t, func(s *provisioners.State) {
		s.Instances["inst-1"] = &provisionerv1.Instance{
			Id:       "inst-1",
			Provider: "runpod",
			Metadata: map[string]*structpb.Value{
				"runpod.machine_id": structpb.NewStringValue("machine-xyz"),
			},
		}
	})

	host, _, endpoint, found, err := svc.LocateEngineNode("inst-1")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if host != "machine-xyz" {
		t.Errorf("host = %q", host)
	}
	if endpoint != "" {
		t.Errorf("endpoint = %q, want empty with no deployment", endpoint)
	}
}
