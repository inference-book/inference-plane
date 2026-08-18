package provisioners

import (
	"context"
	"fmt"
	"log/slog"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/modelstores"
	"github.com/inference-book/inference-plane/internal/vrambudget"
)

// budgetCheck refuses a deploy whose plan cannot fit the cards it is
// about to rent.
//
// The budget and the deploy describe the same plan in two vocabularies
// and nothing compared them, so an operator could size a model at 16k
// with `iplane model budget`, deploy it at 32k, and rent cards that were
// only ever going to hold the smaller plan. The first signal was an
// engine that refused to start, or worse one that started and died when
// the batch filled (#326).
//
// Runs before placement because that is the only place it is worth
// anything. A wrong plan on a single card is a restart; here it is
// several rented cards provisioned against arithmetic the deployment
// then contradicted, discovered after the meter started.
//
// Every input is optional and a missing one skips the check. The plan
// may name no context length, the SKU may have no exact capacity, the
// store may have no hub to ask. A false refusal is worse than the
// silence this replaces, so the check only ever speaks when it has all
// three, and it says which one was missing when it does not.
func (s *Service) budgetCheck(ctx context.Context, req *provisionerv1.CreateDeploymentRequest) error {
	dep := req.GetDeployment()
	if isExternalDeploy(req) {
		// Somebody else's engine on somebody else's hardware. We never
		// chose the cards and refusing on their behalf would be
		// inventing a limit on a machine we cannot see, the same
		// boundary ValidateParallelism draws for cardsKnown.
		return nil
	}

	plan, usable := EnginePlan(dep.GetEngineArgs(), dep.GetParallelism())
	if !usable {
		return nil
	}

	cardBytes, cards := s.deployCardCapacity(req)
	if cardBytes <= 0 {
		s.logBudgetSkip(dep.GetId(), "no exact per-card capacity is recorded for the requested SKU")
		return nil
	}
	if cards > 0 {
		plan.TPSize = cards
	}

	arch := s.modelArchitecture(ctx, dep.GetId(), dep.GetModel())
	if arch == nil {
		return nil
	}

	b, err := vrambudget.Compute(arch, plan)
	if err != nil {
		s.logBudgetSkip(dep.GetId(), err.Error())
		return nil
	}

	usableBytes := vrambudget.UsableBytes(cardBytes, vrambudget.DefaultUtilization)
	switch b.Against(usableBytes) {
	case vrambudget.Overcommitted:
		return fmt.Errorf("this plan does not fit the cards it would rent: %s needs %s per card, and %s holds %s usable at %.2f utilization. %s is the largest claim. Reduce %s, %s, or quantize, or ask for more cards",
			dep.GetModel(), formatGB(b.TotalBytes()), formatGB(cardBytes), formatGB(usableBytes), vrambudget.DefaultUtilization,
			largestBudgetTerm(b), vllmMaxModelLenFlag, vllmMaxNumSeqsFlag)
	case vrambudget.Tight:
		// Tight clears only by eating the overhead band that stands in
		// for the CUDA context and allocator fragmentation. It is a
		// judgement an operator is allowed to make, so it warns rather
		// than refuses; the exit-code gate on `model budget` is the
		// place to be strict, because there nothing has been rented.
		slog.Warn("deployment fits only by consuming its overhead margin",
			"deployment", dep.GetId(), "model", dep.GetModel(),
			"needs_per_card", formatGB(b.TotalBytes()), "usable_per_card", formatGB(usableBytes),
			"largest_term", largestBudgetTerm(b))
	}
	return nil
}

// modelArchitecture reads the model's trained shape from whatever store is
// configured, or nil when it cannot be had.
//
// Shared by the budget check and the expert-shape check so a deploy reads
// the hub once. They ask different questions of the same answer, and two
// fetches would double a network read on the create path for nothing.
//
// nil is "carry on without checking", never "refuse". A store with no hub
// to ask and a hub that describes a model incompletely are both reasons to
// stop checking rather than reasons to stop deploying.
func (s *Service) modelArchitecture(ctx context.Context, deployID, spec string) *provisionerv1.ModelArchitecture {
	src, ok := s.modelStore.(modelstores.ArchitectureSource)
	if !ok {
		return nil
	}
	resp, err := src.Architecture(ctx, &provisionerv1.DescribeModelRequest{ModelSpec: spec})
	if err != nil {
		// The model store already gated the spec at the create pre-flight.
		s.logBudgetSkip(deployID, fmt.Sprintf("the model's shape could not be read: %v", err))
		return nil
	}
	return resp.GetArchitecture()
}

// expertShapeCheck refuses an expert-parallel degree the model's expert
// count does not divide.
//
// Separate from budgetCheck rather than folded into it, because the two
// skip on different things. The budget needs a context length and an exact
// per-card capacity, and gives up without either; this needs only the
// expert count, so a deploy that states no context length still gets the
// expert rule applied.
//
// Applied to an attached engine too. Whether a degree divides an expert
// count is a fact about the weights, not about the machine, so the
// cannot-count-somebody-else's-cards boundary does not reach it.
func (s *Service) expertShapeCheck(ctx context.Context, req *provisionerv1.CreateDeploymentRequest) error {
	dep := req.GetDeployment()
	if dep.GetParallelism().GetExpertParallelSize() <= 1 {
		return nil
	}
	return ValidateExpertShape(dep.GetParallelism(), s.modelArchitecture(ctx, dep.GetId(), dep.GetModel()))
}

// deployCardCapacity resolves what one card of this deploy holds, and
// how many cards one replica gets.
//
// Two paths with two different sources. A pinned instance already
// carries its hardware, in the MiB the adapters normalize to, and that
// figure is exact without needing the catalog at all. An auto-provisioned
// replica has only a SKU token, which the owning provider turns into a
// capacity through the optional CardCapacityReporter.
//
// 0 out means no exact figure, which is unknown and never a card that
// holds nothing.
func (s *Service) deployCardCapacity(req *provisionerv1.CreateDeploymentRequest) (cardBytes int64, cards int32) {
	dep := req.GetDeployment()

	if id := dep.GetInstanceId(); id != "" {
		file, err := s.store.Read()
		if err != nil {
			return 0, 0
		}
		inst, ok := file.Instances[id]
		if !ok {
			return 0, 0
		}
		hw := inst.GetHardware()
		return int64(hw.GetGpuVramMb()) * 1024 * 1024, hw.GetGpuCount()
	}

	for _, spec := range req.GetReplicasSpec() {
		reqs := spec.GetRequirements()
		if reqs.GetSku() == "" {
			continue
		}
		p, ok := s.providers[spec.GetProvider()]
		if !ok {
			continue
		}
		if b := CardCapacityBytes(p, reqs.GetSku()); b > 0 {
			return b, reqs.GetGpuCount()
		}
	}
	return 0, 0
}

// logBudgetSkip records why the check declined to speak. A silent skip
// and a silent pass look identical to an operator who expected a
// pre-flight, so the reason is the whole value of the line.
func (s *Service) logBudgetSkip(deploymentID, reason string) {
	slog.Info("skipping the VRAM pre-flight", "deployment", deploymentID, "reason", reason)
}

// largestBudgetTerm names the claim carrying most of the working set,
// which is what tells an operator what to give up. Past a few thousand
// tokens at any real batch the cache outweighs the weights, so the
// instinct to quantize harder is usually the wrong move.
func largestBudgetTerm(b vrambudget.Budget) string {
	term, biggest := "weights", b.WeightBytes
	if b.KVBytes > biggest {
		term, biggest = "cache", b.KVBytes
	}
	if b.ActivationBytes > biggest {
		term = "activation"
	}
	return term
}

// formatGB renders bytes as the decimal gigabytes vendors quote, so a
// refusal reads in the same unit as the catalog and the budget command.
func formatGB(b int64) string {
	return fmt.Sprintf("%.1f GB", float64(b)/float64(vrambudget.GB))
}
