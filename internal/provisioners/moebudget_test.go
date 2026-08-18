package provisioners_test

import (
	"context"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
)

// glm52Arch is the Part IV rehearsal model's published shape: 753.33B held
// over 78 layers plus one multi-token-prediction block, 256 routed experts
// with 8 active, and a compressed latent cache.
var glm52Arch = &provisionerv1.ModelArchitecture{
	Params: 753_329_940_480, Layers: 78, DenseLayers: 3, MtpLayers: 1,
	HiddenSize: 6144, NumExperts: 256, NumExpertsPerTok: 8, SharedExperts: 1,
	MoeIntermediateSize: 2048, KvLoraRank: 512, QkRopeHeadDim: 64,
	KvHeads: 64, HeadDim: 192, MaxPositionEmbeddings: 1_048_576,
}

func moeDeployReq(id string, engineArgs []string, cards int32) *provisionerv1.CreateDeploymentRequest {
	return &provisionerv1.CreateDeploymentRequest{
		Wait: true,
		Deployment: &provisionerv1.Deployment{
			Id: id, Model: "zai-org/GLM-5.2",
			Image: "vllm/vllm-openai:v0.27.1", EngineArgs: engineArgs,
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider: "cap", Replicas: 1,
			Requirements: &provisionerv1.ResourceRequirements{Sku: "h100", GpuCount: cards},
		}},
	}
}

// TestMoEDeployIsRefusedBeforeAnyProviderCall is the acceptance for #351.
// A 753B model at fp16 clears no eight-card node, and the refusal has to
// land before the rental rather than after, because at this size the box
// being refused costs an order of magnitude more than Ch 12's did.
func TestMoEDeployIsRefusedBeforeAnyProviderCall(t *testing.T) {
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, glm52Arch)

	_, err := svc.CreateDeployment(context.Background(),
		moeDeployReq("glm", []string{"--max-model-len=8192", "--max-num-seqs=8"}, 8))
	if err == nil {
		t.Fatal("accepted a 753B model at fp16 on eight 80 GB cards")
	}
	if got := p.spawnCalls.Load(); got != 0 {
		t.Errorf("provider spawned %d instance(s) before refusing", got)
	}
}

// TestTheMoERefusalNamesTheTermThatFailed: at this scale the weights are
// the whole problem, and saying so is what separates an actionable refusal
// from "does not fit". An operator told only "no" reaches for the context
// length, which is the term that moves least here.
func TestTheMoERefusalNamesTheTermThatFailed(t *testing.T) {
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, glm52Arch)

	_, err := svc.CreateDeployment(context.Background(),
		moeDeployReq("glm", []string{"--max-model-len=8192", "--max-num-seqs=8"}, 8))
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"zai-org/GLM-5.2", "per card", "weights"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q, so it is not actionable: %v", want, err)
		}
	}
}

// TestTheRemedyAgreesWithTheTermThatFailed. Naming the largest claim and
// then advising the knobs that do not move it is worse than saying
// nothing, because an operator follows the sentence in order. At this
// scale the cache is 6 GB of a 223 GB working set, so shrinking the
// context twice gains nothing and looks like a broken tool.
func TestTheRemedyAgreesWithTheTermThatFailed(t *testing.T) {
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, glm52Arch)

	_, err := svc.CreateDeployment(context.Background(),
		moeDeployReq("glm", []string{"--max-model-len=8192", "--max-num-seqs=8"}, 8))
	if err == nil {
		t.Fatal("want a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--quantization") || !strings.Contains(msg, "more cards") {
		t.Errorf("weights-bound refusal does not advise the two things that would help: %v", err)
	}
	// The context and batch knobs may be mentioned as not helping. They
	// must not be offered as the fix.
	if strings.Contains(msg, "Reduce --max-model-len") {
		t.Errorf("weights-bound refusal leads with a knob worth 6 GB of a 223 GB gap: %v", err)
	}
}

// TestAQuantizedMoEPlanIsAccepted is the other half. A budget that refuses
// everything at this scale would be useless, and mxfp4 is the plan the
// model actually fits eight cards under.
func TestAQuantizedMoEPlanIsAccepted(t *testing.T) {
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: b200Bytes}
	svc := budgetService(t, p, glm52Arch)

	if _, err := svc.CreateDeployment(context.Background(),
		moeDeployReq("glm", []string{"--quantization=mxfp4", "--max-model-len=8192", "--max-num-seqs=8"}, 8)); err != nil {
		t.Fatalf("refused a four-bit plan the model fits: %v", err)
	}
}

// TestTheExpertSplitReachesTheBudget pins that --ep composes with the
// refusal rather than bypassing it. The split changes the engine arguments
// the budget reads its plan back out of, so a plan that is checked without
// it is checking a different deployment.
func TestTheExpertSplitReachesTheBudget(t *testing.T) {
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: b200Bytes}
	svc := budgetService(t, p, glm52Arch)

	req := moeDeployReq("glm", []string{"--quantization=mxfp4", "--max-model-len=8192", "--max-num-seqs=8"}, 8)
	req.Deployment.Parallelism = &provisionerv1.Parallelism{TensorParallelSize: 1, ExpertParallelSize: 8}

	resp, err := svc.CreateDeployment(context.Background(), req)
	if err != nil {
		t.Fatalf("refused an expert-parallel plan the model fits: %v", err)
	}
	args := strings.Join(resp.GetDeployment().GetEngineArgs(), " ")
	for _, want := range []string{"--data-parallel-size=8", "--enable-expert-parallel", "--quantization=mxfp4"} {
		if !strings.Contains(args, want) {
			t.Errorf("engine args missing %q; got %q", want, args)
		}
	}
}

var _ = provisioners.ValidateExpertShape

// TestTheExpertSplitChangesWhatTheBudgetSizes is the deploy-path half of
// #376. Under tp=1 the eight-card width is carried by data-parallel ranks,
// each holding whole experts and a full copy of the attention and the
// embeddings, so the same plan needs materially more per card than the old
// divide-everything-by-eight arithmetic said.
func TestTheExpertSplitChangesWhatTheBudgetSizes(t *testing.T) {
	// Eight 80 GB cards at a batch of 32. The old arithmetic put this at
	// 76.0 GB per card and let it through; the corrected one puts it at
	// 91.7 and refuses. That gap is the whole ticket, and it is the
	// direction that rents hardware which then runs out of memory.
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, glm52Arch)

	req := moeDeployReq("glm", []string{"--quantization=mxfp4", "--max-model-len=8192", "--max-num-seqs=32"}, 8)
	req.Deployment.Parallelism = &provisionerv1.Parallelism{TensorParallelSize: 1, ExpertParallelSize: 8}

	_, err := svc.CreateDeployment(context.Background(), req)
	if err == nil {
		t.Fatal("accepted an expert-parallel plan whose replicated weights do not fit 80 GB cards")
	}
	if got := p.spawnCalls.Load(); got != 0 {
		t.Errorf("provider spawned %d instance(s) before refusing", got)
	}
}

// TestADeploymentWithNoExpertSplitIsSizedAsBefore pins the compatibility
// promise. The card count still stands in for the tensor split everywhere
// that expert parallelism was not asked for.
func TestADeploymentWithNoExpertSplitIsSizedAsBefore(t *testing.T) {
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, glm52Arch)

	// The same plan that the expert-parallel case above is refused for.
	// Naming no split still means sized against the box, so it fits.
	if _, err := svc.CreateDeployment(context.Background(),
		moeDeployReq("glm", []string{"--quantization=mxfp4", "--max-model-len=8192", "--max-num-seqs=32"}, 8)); err != nil {
		t.Fatalf("a deployment naming no split is now refused: %v", err)
	}
}
