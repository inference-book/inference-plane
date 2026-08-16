package provisioners_test

import (
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// The check exists because the engine cannot make it in time. vLLM discovers a
// world size larger than the visible device count when it starts, which is
// after the rental began billing; the control plane knows the card count
// before renting anything.
func TestParallelismRefusesASplitTheCardsCannotCarry(t *testing.T) {
	_, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{TensorParallelSize: 4, PipelineParallelSize: 2}, 4, true)

	if err == nil {
		t.Fatal("accepted an eight-way split on four cards")
	}
	for _, want := range []string{"needs 8 cards", "asks for 4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q, so an operator cannot see the arithmetic: %v", want, err)
		}
	}
}

// tp and pp multiply rather than add: a four-way tensor split inside each of
// two pipeline stages is eight cards, not six.
func TestParallelismMultipliesTheTwoDimensions(t *testing.T) {
	if _, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{TensorParallelSize: 4, PipelineParallelSize: 2}, 8, true); err != nil {
		t.Errorf("rejected an eight-way split on eight cards: %v", err)
	}
}

// An unset dimension is one way, not zero ways. Treating it as zero would make
// the product zero and let any split through.
func TestParallelismTreatsAnUnsetDimensionAsOne(t *testing.T) {
	args, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{TensorParallelSize: 4}, 4, true)
	if err != nil {
		t.Fatalf("tp=4 on four cards: %v", err)
	}
	if len(args) != 1 || !strings.Contains(args[0], "tensor-parallel-size=4") {
		t.Errorf("args = %v, want only the tensor-parallel flag", args)
	}

	if _, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{TensorParallelSize: 8}, 4, true); err == nil {
		t.Error("accepted tp=8 on four cards, so an unset pp is being read as zero rather than one")
	}
}

// A deployment that asks for no split says nothing to the engine, which is
// what every single-card deployment since Chapter 6 has done.
func TestParallelismUnsetEmitsNothing(t *testing.T) {
	args, err := provisioners.ValidateParallelism(nil, 1, true)
	if err != nil || args != nil {
		t.Errorf("got %v, %v; want no args and no error for an unset split", args, err)
	}
}

// A one-way split is the default the engine would pick anyway, so saying it
// explicitly is noise on the command line.
func TestParallelismOmitsOneWaySplits(t *testing.T) {
	args, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{TensorParallelSize: 1, PipelineParallelSize: 1}, 1, true)
	if err != nil {
		t.Fatalf("1x1 on one card: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want none: a one-way split is what the engine does unasked", args)
	}
}

// Absent gpu_count means one card, matching the rest of the provisioning path.
func TestParallelismDefaultsToOneCard(t *testing.T) {
	if _, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{TensorParallelSize: 2}, 0, true); err == nil {
		t.Error("accepted a two-way split with no gpu_count stated; absent means one card")
	}
}

// An engine iplane did not provision is the operator's machine. We never saw
// its cards, so refusing a split because we cannot count them would be
// inventing a limit on somebody else's hardware. The flags still go through,
// because passing them on is the whole job for an attached engine.
func TestParallelismDoesNotPoliceHardwareItDidNotRent(t *testing.T) {
	args, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{TensorParallelSize: 8}, 0, false)
	if err != nil {
		t.Fatalf("refused a split on an attached engine: %v", err)
	}
	if len(args) != 1 || !strings.Contains(args[0], "tensor-parallel-size=8") {
		t.Errorf("args = %v, want the flag passed through unchecked", args)
	}
}
