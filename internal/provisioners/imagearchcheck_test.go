package provisioners_test

import (
	"context"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// stubArchSource stands in for the registry read, recording what it was
// asked so a test can assert the call was avoided when it would be wasted.
type stubArchSource struct {
	arches []string
	why    string
	asked  []string
}

func (s *stubArchSource) Architectures(_ context.Context, image string) ([]string, string) {
	s.asked = append(s.asked, image)
	return s.arches, s.why
}

func archSvc(t *testing.T, src provisioners.ImageArchSource) (*provisioners.Service, *specRecordingProvider) {
	t.Helper()
	prov := &specRecordingProvider{mockProvider: &mockProvider{name: "vmstyle"}}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithDeploymentExecutor(&recordingExecutor{}),
		provisioners.WithImageArchSource(src))
	return svc, prov
}

func archDeployReq(arch string) *provisionerv1.CreateDeploymentRequest {
	return &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: "my-llama", Image: "vllm/vllm-openai:v0.7.0",
			Model: "Qwen/Qwen2.5-0.5B-Instruct", EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:     "vmstyle",
			Region:       "us-east-1",
			Requirements: &provisionerv1.ResourceRequirements{Sku: "mock-sku", Architecture: arch},
			Replicas:     1,
		}},
		Wait: true,
	}
}

// The trap #405 describes: an operator who does not know their image is x86
// rents Lambda's arm64 GH200 and finds out when the container will not start
// on a machine that is already billing. The image knows, so ask it.
func TestDeployInfersTheArchitectureTheImageNeeds(t *testing.T) {
	src := &stubArchSource{arches: []string{"amd64"}}
	svc, prov := archSvc(t, src)

	if _, err := svc.CreateDeployment(context.Background(), archDeployReq("")); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got := prov.lastSpec(t).GetRequirements().GetArchitecture(); got != provisioners.ArchAMD64 {
		t.Errorf("requirements architecture = %q, want amd64 read from the image", got)
	}
	if len(src.asked) != 1 || src.asked[0] != "vllm/vllm-openai:v0.7.0" {
		t.Errorf("asked = %v, want the engine image once", src.asked)
	}
}

// An operator who passed --arch has made a claim about their own image that
// may be better informed than a manifest, since a cross-built image is a real
// thing. The registry is not asked at all, which also saves a call.
func TestDeployDoesNotOverrideAStatedArchitecture(t *testing.T) {
	src := &stubArchSource{arches: []string{"amd64"}}
	svc, prov := archSvc(t, src)

	if _, err := svc.CreateDeployment(context.Background(), archDeployReq(provisioners.ArchARM64)); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got := prov.lastSpec(t).GetRequirements().GetArchitecture(); got != provisioners.ArchARM64 {
		t.Errorf("requirements architecture = %q, want the operator's arm64 kept", got)
	}
	if len(src.asked) != 0 {
		t.Errorf("asked the registry %v when the operator had already answered", src.asked)
	}
}

// An image that runs on everything constrains nothing, and
// ResourceRequirements.architecture is a single value. Recording one of the
// two would be a narrower claim than the image makes, and would exclude
// hosts that would have worked.
func TestDeployLeavesAMultiArchImageUnconstrained(t *testing.T) {
	src := &stubArchSource{arches: []string{"amd64", "arm64"}}
	svc, prov := archSvc(t, src)

	if _, err := svc.CreateDeployment(context.Background(), archDeployReq("")); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got := prov.lastSpec(t).GetRequirements().GetArchitecture(); got != "" {
		t.Errorf("requirements architecture = %q, want unconstrained for a multi-arch image", got)
	}
}

// An unreadable registry, a rate limit and a private image are one outcome:
// nothing was learned. Refusing there would block deploys that work today
// over a network call that is new.
func TestDeployProceedsWhenTheRegistryCannotBeRead(t *testing.T) {
	src := &stubArchSource{why: "registry answered 429"}
	svc, prov := archSvc(t, src)

	if _, err := svc.CreateDeployment(context.Background(), archDeployReq("")); err != nil {
		t.Fatalf("CreateDeployment refused over an unreadable registry: %v", err)
	}
	if got := prov.lastSpec(t).GetRequirements().GetArchitecture(); got != "" {
		t.Errorf("requirements architecture = %q, want unconstrained", got)
	}
}

// A Service with no source behaves exactly as it did before this existed.
func TestDeployWithoutASourceIsUnchanged(t *testing.T) {
	svc, prov := archSvc(t, nil)
	if _, err := svc.CreateDeployment(context.Background(), archDeployReq("")); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got := prov.lastSpec(t).GetRequirements().GetArchitecture(); got != "" {
		t.Errorf("requirements architecture = %q, want unset", got)
	}
}

// A vocabulary iplane has no name for is silence, not a value to record. An
// unrecognized string would reach skucatalog.Match and exclude every shape,
// turning a readable registry into a deploy that cannot be satisfied.
func TestDeployIgnoresAnArchitectureItDoesNotKnow(t *testing.T) {
	src := &stubArchSource{arches: []string{"riscv64"}}
	svc, prov := archSvc(t, src)

	if _, err := svc.CreateDeployment(context.Background(), archDeployReq("")); err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
	if got := prov.lastSpec(t).GetRequirements().GetArchitecture(); got != "" {
		t.Errorf("requirements architecture = %q, want unconstrained for an unknown vocabulary", got)
	}
}

// Somebody else's engine on somebody else's hardware. We never chose the
// cards, so there is nothing to constrain and no reason to spend a call.
func TestDeployDoesNotAskForAnExternalDeploy(t *testing.T) {
	src := &stubArchSource{arches: []string{"amd64"}}
	svc, _ := archSvc(t, src)

	_, _ = svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: "attached", Image: "vllm/vllm-openai:v0.7.0",
			Model: "Qwen/Qwen2.5-0.5B-Instruct", EnginePort: 8000,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:       provisioners.ProviderExternal,
			EngineEndpoint: "http://10.0.0.9:8000",
			Replicas:       1,
		}},
	})
	if len(src.asked) != 0 {
		t.Errorf("asked %v for a deploy onto hardware iplane did not choose", src.asked)
	}
}
