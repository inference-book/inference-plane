package vrambudget_test

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

// The weight terms of GLM-5.2 at fp16, exactly. 76 expert layers of 256
// experts, each 3 x 6144 x 2048 parameters.
const (
	glmRoutedParams    int64 = 76 * 256 * 3 * 6144 * 2048
	glmNonRoutedParams int64 = 753_329_940_480 - glmRoutedParams
)

func fp16Plan(tp, ep int32) vrambudget.Plan {
	return vrambudget.Plan{
		Weights: vrambudget.PrecisionFP16, MaxModelLen: 8192, MaxBatch: 8,
		TPSize: tp, EPSize: ep,
	}
}

// TestExpertParallelReplicatesWhatItDoesNotShard is the defect. Under
// tp=1 with the width carried by data parallelism, the routed experts
// shard across the eight ranks and everything else is replicated on every
// one of them. Dividing the whole weight by eight understates the
// attention, the embeddings and the dense layers eightfold.
func TestExpertParallelReplicatesWhatItDoesNotShard(t *testing.T) {
	b, err := vrambudget.Compute(glm52(), fp16Plan(1, 8))
	if err != nil {
		t.Fatal(err)
	}

	want := glmRoutedParams*2/8 + glmNonRoutedParams*2
	if b.WeightBytes != want {
		t.Errorf("WeightBytes = %d (%.1f GB), want %d (%.1f GB)",
			b.WeightBytes, float64(b.WeightBytes)/1e9, want, float64(want)/1e9)
	}
}

// TestTensorParallelAtFullWidthIsUnchanged: when the tensor split covers
// every card there is no data-parallel replication, so every term divides
// by the card count exactly as before.
func TestTensorParallelAtFullWidthIsUnchanged(t *testing.T) {
	b, err := vrambudget.Compute(glm52(), fp16Plan(8, 8))
	if err != nil {
		t.Fatal(err)
	}

	want := int64(753_329_940_480) * 2 / 8
	if b.WeightBytes != want {
		t.Errorf("WeightBytes = %d, want %d (the whole model over eight cards)", b.WeightBytes, want)
	}
}

// TestAnUnsetExpertSizeBehavesExactlyAsBefore is the compatibility proof.
// Every deployment that does not ask for expert parallelism computes the
// number it computed yesterday.
func TestAnUnsetExpertSizeBehavesExactlyAsBefore(t *testing.T) {
	withEP, err := vrambudget.Compute(glm52(), fp16Plan(8, 0))
	if err != nil {
		t.Fatal(err)
	}
	// The same plan expressed without the field at all.
	legacy, err := vrambudget.Compute(glm52(), vrambudget.Plan{
		Weights: vrambudget.PrecisionFP16, MaxModelLen: 8192, MaxBatch: 8, TPSize: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	if withEP.WeightBytes != legacy.WeightBytes {
		t.Errorf("EPSize=0 changed the answer: %d vs %d", withEP.WeightBytes, legacy.WeightBytes)
	}
	// And the absolute figure, not only that the two agree. Comparing two
	// plans that are equivalent by construction would pass even if the
	// unset case fell back to the wrong divisor, which is exactly what a
	// mutation of expertCards' fallback does.
	want := int64(753_329_940_480) * 2 / 8
	if legacy.WeightBytes != want {
		t.Errorf("WeightBytes = %d, want %d (the whole model over the tensor width)", legacy.WeightBytes, want)
	}
}

// TestADenseModelIgnoresTheExpertSize: no routed experts means nothing to
// shard differently, so the split collapses back to one division.
func TestADenseModelIgnoresTheExpertSize(t *testing.T) {
	dense := &provisionerv1.ModelArchitecture{
		Params: 70_600_000_000, Layers: 80, KvHeads: 8, HeadDim: 128,
		HiddenSize: 8192, MaxPositionEmbeddings: 131_072,
	}
	sharded, err := vrambudget.Compute(dense, fp16Plan(4, 8))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := vrambudget.Compute(dense, fp16Plan(4, 0))
	if err != nil {
		t.Fatal(err)
	}
	if sharded.WeightBytes != plain.WeightBytes {
		t.Errorf("EPSize changed a dense model's weights: %d vs %d", sharded.WeightBytes, plain.WeightBytes)
	}
	if want := int64(70_600_000_000) * 2 / 4; plain.WeightBytes != want {
		t.Errorf("WeightBytes = %d, want %d", plain.WeightBytes, want)
	}
}
