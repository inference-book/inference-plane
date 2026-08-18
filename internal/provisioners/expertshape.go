package provisioners

import (
	"fmt"
	"strconv"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// maxArrangeableEPDegree bounds the working degrees a refusal suggests.
//
// A split cannot span instances yet (issue 212), so an expert degree above
// the cards on one machine is not arrangeable however well it divides. 64
// is generous for a single node and keeps the suggestion list short enough
// to read; 896 has sixteen divisors and naming all of them would be a
// factorisation, not advice.
const maxArrangeableEPDegree = 32

// ValidateExpertShape refuses an expert-parallel degree the model's expert
// count does not divide.
//
// An engine places whole experts on each rank, so a degree that does not
// divide the count leaves some ranks holding one more expert than others.
// The per-card budget is then computed against the average while one card
// carries the maximum, and the first symptom is that card running out of
// memory. That happens after the rental started, several minutes after the
// operator typed the command, which is exactly the window a pre-rent
// refusal exists to cover.
//
// Distinct from the tp-divides-ep rule in ValidateParallelism. That one is
// about what an engine can arrange from a tensor width and a data-parallel
// width, and holds for any model. This one is about the weights, and needs
// the model's shape read from the hub.
//
// Fails open on every missing input. A dense model has no experts to place
// unevenly, a request with no expert split described nothing to check, and
// an architecture the hub describes incompletely is a reason to stop
// checking rather than a reason to stop deploying. Same boundary
// budgetCheck draws, for the same reason: a false refusal is worse than
// the silence it replaces.
func ValidateExpertShape(par *provisionerv1.Parallelism, arch *provisionerv1.ModelArchitecture) error {
	ep := par.GetExpertParallelSize()
	if ep <= 1 {
		return nil
	}
	experts := arch.GetNumExperts()
	if experts <= 0 {
		return nil
	}
	if experts%ep == 0 {
		return nil
	}
	return fmt.Errorf(
		"parallelism: expert_parallel_size %d does not divide the model's %d routed experts, so some cards would hold "+
			"%d experts and others %d, and the per-card budget would be computed against the average while one card "+
			"carries the maximum; %s",
		ep, experts, experts/ep+1, experts/ep, suggestEPDegrees(experts))
}

// suggestEPDegrees names the degrees that would divide evenly, so the
// refusal is actionable without the operator factorising the expert count.
func suggestEPDegrees(experts int32) string {
	var ds []string
	for d := int32(2); d <= maxArrangeableEPDegree && d <= experts; d++ {
		if experts%d == 0 {
			ds = append(ds, strconv.FormatInt(int64(d), 10))
		}
	}
	if len(ds) == 0 {
		return fmt.Sprintf("no degree up to %d divides %d, so this model wants a wider split than one machine holds", maxArrangeableEPDegree, experts)
	}
	return "these divide it: " + strings.Join(ds, ", ")
}
