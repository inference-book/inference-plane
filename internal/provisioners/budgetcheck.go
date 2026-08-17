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

	src, ok := s.modelStore.(modelstores.ArchitectureSource)
	if !ok {
		return nil
	}
	resp, err := src.Architecture(ctx, &provisionerv1.DescribeModelRequest{ModelSpec: dep.GetModel()})
	if err != nil {
		// The model store already gated the spec at the pre-flight above.
		// A shape it cannot read here is a model the hub describes
		// incompletely, which is a reason to stop checking rather than a
		// reason to stop deploying.
		s.logBudgetSkip(dep.GetId(), fmt.Sprintf("the model's shape could not be read: %v", err))
		return nil
	}

	b, err := vrambudget.Compute(resp.GetArchitecture(), plan)
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
