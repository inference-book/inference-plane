package provisioners_test

import (
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

func argsFor(t *testing.T, p *provisionerv1.Parallelism, cards int32) []string {
	t.Helper()
	args, err := provisioners.ValidateParallelism(p, cards, true)
	if err != nil {
		t.Fatalf("ValidateParallelism(%v, %d): %v", p, cards, err)
	}
	return args
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// TestExpertParallelEmitsTheFlagsTheEngineActuallyTakes is the whole point of
// the mapping. vLLM has no --expert-parallel-size: it switches expert
// parallelism on with a boolean and derives the degree as tp x dp. Emitting a
// size flag would fail the engine's argument parse after the meter started.
func TestExpertParallelEmitsTheFlagsTheEngineActuallyTakes(t *testing.T) {
	args := argsFor(t, &provisionerv1.Parallelism{TensorParallelSize: 1, ExpertParallelSize: 8}, 8)

	for _, want := range []string{
		"--tensor-parallel-size=1",
		"--data-parallel-size=8",
		"--enable-expert-parallel",
	} {
		if !hasArg(args, want) {
			t.Errorf("missing %q; got %v", want, args)
		}
	}
	for _, banned := range args {
		if strings.HasPrefix(banned, "--expert-parallel-size") {
			t.Errorf("emitted %q, which vLLM does not accept", banned)
		}
	}
}

// TestExpertParallelDerivesTheDataParallelWidth covers the arithmetic an
// operator would otherwise have to do. They say how many ways to spread the
// experts; tp is already declared, so dp follows.
func TestExpertParallelDerivesTheDataParallelWidth(t *testing.T) {
	args := argsFor(t, &provisionerv1.Parallelism{TensorParallelSize: 2, ExpertParallelSize: 8}, 8)
	if !hasArg(args, "--data-parallel-size=4") {
		t.Errorf("tp 2 with ep 8 should give dp 4; got %v", args)
	}
}

// TestExpertParallelRefusesADegreeTPDoesNotDivide: dp has to be a whole
// number, and the engine would otherwise discover the mismatch on startup.
func TestExpertParallelRefusesADegreeTPDoesNotDivide(t *testing.T) {
	_, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{TensorParallelSize: 3, ExpertParallelSize: 8}, 8, true)
	if err == nil {
		t.Fatal("accepted ep 8 with tp 3, which has no whole data-parallel width")
	}
	for _, want := range []string{"expert_parallel_size 8", "tensor_parallel_size 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q, so the operator cannot see why: %v", want, err)
		}
	}
}

// TestExpertParallelCountsTheDataParallelRanksAgainstTheCards is the
// correctness case that makes --ep worth having. tp 1 with ep 8 runs eight
// data-parallel ranks and needs eight cards; counting only tp would call it
// one and let the deploy through.
func TestExpertParallelCountsTheDataParallelRanksAgainstTheCards(t *testing.T) {
	_, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{TensorParallelSize: 1, ExpertParallelSize: 8}, 4, true)
	if err == nil {
		t.Fatal("accepted eight data-parallel ranks on four cards")
	}
	if !strings.Contains(err.Error(), "needs 8 cards") {
		t.Errorf("error does not show the eight-card requirement: %v", err)
	}
}

// TestOneWayExpertParallelEmitsNothing: spreading experts one way is what the
// engine does unasked, same rule the tp and pp dimensions already follow.
func TestOneWayExpertParallelEmitsNothing(t *testing.T) {
	args := argsFor(t, &provisionerv1.Parallelism{ExpertParallelSize: 1}, 1)
	for _, a := range args {
		if strings.Contains(a, "expert") || strings.Contains(a, "data-parallel") {
			t.Errorf("a one-way expert split emitted %q", a)
		}
	}
}

func TestExpertParallelRejectsANegativeDegree(t *testing.T) {
	if _, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{ExpertParallelSize: -2}, 8, true); err == nil {
		t.Fatal("accepted a negative expert-parallel size")
	}
}

// TestExpertParallelOnHardwareWeDidNotRentStillMaps: the cards cannot be
// counted on an attached engine, so the card check is skipped, but the flag
// translation is the whole job for a deployment we are only forwarding to.
func TestExpertParallelOnHardwareWeDidNotRentStillMaps(t *testing.T) {
	args, err := provisioners.ValidateParallelism(
		&provisionerv1.Parallelism{TensorParallelSize: 1, ExpertParallelSize: 8}, 0, false)
	if err != nil {
		t.Fatalf("refused a split on hardware it cannot count: %v", err)
	}
	if !hasArg(args, "--enable-expert-parallel") || !hasArg(args, "--data-parallel-size=8") {
		t.Errorf("did not forward the expert split; got %v", args)
	}
}
