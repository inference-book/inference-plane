package vrambudget_test

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

// glm52 is the published shape of zai-org/GLM-5.2, the Part IV rehearsal
// model. Params is HF's safetensors accounting, which counts the MTP
// layer's experts; num_hidden_layers does not.
func glm52() *provisionerv1.ModelArchitecture {
	return &provisionerv1.ModelArchitecture{
		Params:                753_329_940_480,
		Layers:                78,
		DenseLayers:           3,
		HiddenSize:            6144,
		NumExperts:            256,
		NumExpertsPerTok:      8,
		SharedExperts:         1,
		MoeIntermediateSize:   2048,
		MtpLayers:             1,
		KvLoraRank:            512,
		QkRopeHeadDim:         64,
		MaxPositionEmbeddings: 1_048_576,
	}
}

// TestMTPLayerIsCountedAmongTheExpertLayers is the defect. The MTP block
// holds a full 256-expert stack that the parameter total counts and
// num_hidden_layers does not, so sizing the routed share from the layer
// count alone leaves one layer of unpicked experts in the activated
// figure.
func TestMTPLayerIsCountedAmongTheExpertLayers(t *testing.T) {
	got := vrambudget.ActiveParams(glm52())

	// 76 expert layers (78 hidden - 3 dense + 1 mtp), each holding 248
	// unpicked experts of 3 x 6144 x 2048 parameters.
	const want = 41_841_764_352
	if got != want {
		t.Errorf("ActiveParams = %d (%.2f B), want %d (%.2f B)",
			got, float64(got)/1e9, want, float64(want)/1e9)
	}
}

// TestWithoutTheMTPLayerTheFigureIsUnchanged pins that the correction is
// scoped to models that publish one. Kimi K3 reports zero, which is why
// the formula validated exactly against it and the error survived.
func TestWithoutTheMTPLayerTheFigureIsUnchanged(t *testing.T) {
	a := glm52()
	a.MtpLayers = 0

	const want = 51_203_450_880 // 75 expert layers
	if got := vrambudget.ActiveParams(a); got != want {
		t.Errorf("ActiveParams = %d, want %d for a model with no MTP layer", got, want)
	}
}

// TestMTPLayerRaisesTheResidentExpertShare: the MTP block's experts sit on
// the cards whether or not speculative decoding runs, so they belong in
// the resident figure too.
func TestMTPLayerRaisesTheResidentExpertShare(t *testing.T) {
	with := vrambudget.RoutedExpertParams(glm52())

	a := glm52()
	a.MtpLayers = 0
	without := vrambudget.RoutedExpertParams(a)

	oneLayer := int64(256) * 3 * 6144 * 2048
	if with-without != oneLayer {
		t.Errorf("MTP layer contributed %d resident expert params, want %d", with-without, oneLayer)
	}
}

// TestMTPLayerDoesNotAffectADenseModel keeps the correction out of the
// dense path, where there are no experts to apportion.
func TestMTPLayerDoesNotAffectADenseModel(t *testing.T) {
	dense := &provisionerv1.ModelArchitecture{
		Params: 70_600_000_000, Layers: 80, KvHeads: 8, HeadDim: 128,
		HiddenSize: 8192, MaxPositionEmbeddings: 131_072, MtpLayers: 1,
	}
	if got := vrambudget.ActiveParams(dense); got != 70_600_000_000 {
		t.Errorf("ActiveParams = %d, want the full count for a dense model", got)
	}
}
