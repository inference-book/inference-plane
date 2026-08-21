package provisioners_test

import (
	"context"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// archOnlyStore answers the architecture capability with a checkpoint that
// already chose its precision, which is what every published four-bit
// build of a frontier model is.
type archOnlyStore struct {
	arch *provisionerv1.ModelArchitecture
}

func (a *archOnlyStore) Resolve(_ context.Context, spec string) (modelstores.Resolved, error) {
	return modelstores.Resolved{EngineModelArg: spec}, nil
}

func (a *archOnlyStore) Architecture(context.Context, *provisionerv1.DescribeModelRequest) (*provisionerv1.DescribeModelResponse, error) {
	return &provisionerv1.DescribeModelResponse{Architecture: a.arch}, nil
}

// The deploy path sized a pre-quantized checkpoint at the engine's default
// precision and refused a plan that fits comfortably.
//
// cyankiwi/GLM-5.2-AWQ-INT4 is 474 GB of four-bit weights, 59 GB per card
// across eight. The hub reports its parameter count as 753.33B, so pricing
// it at fp16 gives 188 GB per card and the check refuses a deploy that
// would have worked. `iplane model budget` learned this in #382 and
// refuses to price such a checkpoint at all; the deploy path never did.
func TestBudgetCheckSkipsACheckpointThatAlreadyChoseItsPrecision(t *testing.T) {
	arch := &provisionerv1.ModelArchitecture{
		Params: 753_329_940_480, Layers: 78, HiddenSize: 6144,
		KvLoraRank: 512, QkRopeHeadDim: 64, MaxPositionEmbeddings: 1_048_576,
		Quantization: "compressed-tensors",
	}
	store, err := file.Open(t.TempDir(), "default")
	if err != nil {
		t.Fatalf("file.Open: %v", err)
	}
	// The check only runs when the provider can say what one card holds,
	// which is exactly the case a real deploy with --sku hits.
	prov := &capacityReportingProvider{fanOutMockProvider: &fanOutMockProvider{name: "mockfan"}}
	svc := provisioners.New([]provisioners.Provider{prov}, store, "default",
		provisioners.WithKeyStore(newKeyStore(t)),
		provisioners.WithModelStore(&archOnlyStore{arch: arch}))

	_, err = svc.CreateDeployment(context.Background(), &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id: "glm", Image: "vllm/vllm-openai:v0.27.1", Model: "cyankiwi/GLM-5.2-AWQ-INT4",
			EnginePort:  8000,
			EngineArgs:  []string{"--max-model-len", "32768", "--max-num-seqs", "32"},
			Parallelism: &provisionerv1.Parallelism{TensorParallelSize: 8},
		},
		ReplicasSpec: []*provisionerv1.ReplicaSpec{{
			Provider:     "mockfan",
			Requirements: &provisionerv1.ResourceRequirements{Sku: "H100_SXM", GpuCount: 8, MinVramGb: 80},
			Replicas:     1,
		}},
		Wait: true,
	})
	if err != nil && strings.Contains(err.Error(), "does not fit the cards") {
		t.Fatalf("refused a four-bit checkpoint by pricing it at the engine default: %v", err)
	}
	if err != nil {
		t.Fatalf("CreateDeployment: %v", err)
	}
}

// capacityReportingProvider is the mock plus CardCapacityReporter, since
// the VRAM pre-flight declines to speak without an exact per-card figure
// and would otherwise skip for the wrong reason.
type capacityReportingProvider struct{ *fanOutMockProvider }

func (c *capacityReportingProvider) CardCapacityBytes(string) int64 { return 80 << 30 }
