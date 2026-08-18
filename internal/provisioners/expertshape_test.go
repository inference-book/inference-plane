package provisioners_test

import (
	"context"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
)

// kimiK3 is the shape the rule exists for: 896 routed experts, 8 active.
func kimiK3() *provisionerv1.ModelArchitecture {
	return &provisionerv1.ModelArchitecture{NumExperts: 896, NumExpertsPerTok: 8}
}

func TestExpertShapeRefusesADegreeTheExpertCountDoesNotDivide(t *testing.T) {
	err := provisioners.ValidateExpertShape(
		&provisionerv1.Parallelism{ExpertParallelSize: 6}, kimiK3())
	if err == nil {
		t.Fatal("accepted ep 6 against 896 experts, which places them unevenly")
	}
	for _, want := range []string{"896", "6"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not name %q: %v", want, err)
		}
	}
}

// TestExpertShapeNamesDegreesThatWouldWork is what makes the refusal
// actionable. An operator told only "no" has to factorise 896 themselves.
func TestExpertShapeNamesDegreesThatWouldWork(t *testing.T) {
	err := provisioners.ValidateExpertShape(
		&provisionerv1.Parallelism{ExpertParallelSize: 6}, kimiK3())
	if err == nil {
		t.Fatal("want a refusal")
	}
	// 896 = 2^7 x 7, so these are the arrangeable degrees at node scale.
	for _, want := range []string{"7", "8", "14", "16"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not offer %q as a working degree: %v", want, err)
		}
	}
}

// TestExpertShapeAcceptsADivisor covers the ticket's own example, which is
// arithmetically a pass rather than a refusal: 896 = 7 x 128.
func TestExpertShapeAcceptsADivisor(t *testing.T) {
	for _, ep := range []int32{1, 2, 4, 7, 8, 14, 16, 112, 128, 896} {
		if err := provisioners.ValidateExpertShape(
			&provisionerv1.Parallelism{ExpertParallelSize: ep}, kimiK3()); err != nil {
			t.Errorf("ep %d divides 896 and was refused: %v", ep, err)
		}
	}
}

// TestExpertShapeSkipsDenseModels: a model with no experts has nothing to
// place unevenly, and refusing there would invent a rule about a shape the
// model does not have.
func TestExpertShapeSkipsDenseModels(t *testing.T) {
	if err := provisioners.ValidateExpertShape(
		&provisionerv1.Parallelism{ExpertParallelSize: 6},
		&provisionerv1.ModelArchitecture{NumExperts: 0}); err != nil {
		t.Errorf("refused a dense model on an expert rule: %v", err)
	}
}

// TestExpertShapeSkipsWhenNoExpertSplitWasAskedFor keeps the rule scoped to
// what the operator described. One way is not a split.
func TestExpertShapeSkipsWhenNoExpertSplitWasAskedFor(t *testing.T) {
	for _, par := range []*provisionerv1.Parallelism{
		nil,
		{},
		{ExpertParallelSize: 1},
		{TensorParallelSize: 8},
	} {
		if err := provisioners.ValidateExpertShape(par, kimiK3()); err != nil {
			t.Errorf("refused %v with no expert split requested: %v", par, err)
		}
	}
}

// TestExpertShapeSkipsANonsenseExpertCount is what the experts <= 0 guard
// is actually for. A dense model reports zero and the modulo would let that
// through on its own, but a negative count from malformed hub data would
// not: Go's remainder keeps the sign, so -1 % 6 is -1 and the check would
// refuse a deploy while claiming the model has minus one expert.
func TestExpertShapeSkipsANonsenseExpertCount(t *testing.T) {
	if err := provisioners.ValidateExpertShape(
		&provisionerv1.Parallelism{ExpertParallelSize: 6},
		&provisionerv1.ModelArchitecture{NumExperts: -1}); err != nil {
		t.Errorf("refused a deploy over a negative expert count: %v", err)
	}
}

// TestExpertShapeSkipsAnUnreadableArchitecture is the fail-open case. A hub
// that describes a model incompletely is a reason to stop checking, not a
// reason to stop deploying, which is the rule budgetCheck already follows.
func TestExpertShapeSkipsAnUnreadableArchitecture(t *testing.T) {
	if err := provisioners.ValidateExpertShape(
		&provisionerv1.Parallelism{ExpertParallelSize: 6}, nil); err != nil {
		t.Errorf("refused a deploy because the model shape could not be read: %v", err)
	}
}

// TestDeployRefusesAnUnevenExpertSplitBeforeRenting is the whole point.
// The arithmetic is checked in the unit tests above; this proves it runs on
// the create path and that nothing is rented when it fails.
func TestDeployRefusesAnUnevenExpertSplitBeforeRenting(t *testing.T) {
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, kimiK3())

	req := budgetDeployReq("k3", nil, 8)
	req.Deployment.Parallelism = &provisionerv1.Parallelism{TensorParallelSize: 1, ExpertParallelSize: 6}

	_, err := svc.CreateDeployment(context.Background(), req)
	if err == nil {
		t.Fatal("want a refusal for ep 6 against 896 experts")
	}
	if !strings.Contains(err.Error(), "896 routed experts") {
		t.Errorf("refusal does not name the expert count: %v", err)
	}
	if got := p.spawnCalls.Load(); got != 0 {
		t.Errorf("provider spawned %d instance(s) before refusing", got)
	}
}

// TestDeployAcceptsAnEvenExpertSplit is the same path with a degree that
// divides, so a passing case proves the gate is not simply refusing every
// expert-parallel deploy.
func TestDeployAcceptsAnEvenExpertSplit(t *testing.T) {
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, kimiK3())

	req := budgetDeployReq("k3", nil, 8)
	req.Deployment.Parallelism = &provisionerv1.Parallelism{TensorParallelSize: 1, ExpertParallelSize: 8}

	resp, err := svc.CreateDeployment(context.Background(), req)
	if err != nil {
		t.Fatalf("refused ep 8 against 896 experts: %v", err)
	}
	// And the translation reached the record, which is what the engine
	// will be handed.
	args := strings.Join(resp.GetDeployment().GetEngineArgs(), " ")
	for _, want := range []string{"--data-parallel-size=8", "--enable-expert-parallel"} {
		if !strings.Contains(args, want) {
			t.Errorf("engine args missing %q; got %q", want, args)
		}
	}
}

// TestDeployStillChecksExpertsWhenTheBudgetCannotRun keeps the two gates
// independent. A deploy naming no context length skips the budget entirely,
// and the expert rule needs no context length to be right.
func TestDeployStillChecksExpertsWhenTheBudgetCannotRun(t *testing.T) {
	p := &capacityProvider{Provider: local.New(), name: "cap", bytes: a100Bytes}
	svc := budgetService(t, p, kimiK3())

	// No --max-model-len, so EnginePlan reports unusable and budgetCheck
	// returns early.
	req := budgetDeployReq("k3", []string{"--quantization=fp8"}, 8)
	req.Deployment.Parallelism = &provisionerv1.Parallelism{TensorParallelSize: 1, ExpertParallelSize: 6}

	if _, err := svc.CreateDeployment(context.Background(), req); err == nil {
		t.Fatal("the expert rule did not run when the budget check skipped")
	}
	if got := p.spawnCalls.Load(); got != 0 {
		t.Errorf("provider spawned %d instance(s) before refusing", got)
	}
}
