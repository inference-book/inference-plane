package provisioners

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Placement is the generated provisionerv1.Placement. See Candidate in
// candidates.go for why these are aliases.
type Placement = provisionerv1.Placement

// maxRunners bounds what a placement carries. Enough to see whether the
// decision was close, not so many that the answer becomes another list to
// read.
const maxRunners = 3

// SelectCheapest asks the given providers for candidates and picks the
// cheapest that fits.
//
// This is the difference between price as a report and price as a placement
// input. Ranking a list an operator then reads and acts on by hand is the
// former; the resolver actually choosing is the latter, and the reason it
// wants doing at the seam rather than in the CLI is that every path that
// provisions should be able to reach the same decision.
//
// Cheapest is not the same as best, and the gap is why Placement carries its
// reasoning rather than just an answer. A candidate can be cheapest because it
// is in a region where the model is not staged, in which case the cold start
// costs more than the hourly saving, which is the Ch 9 finding arriving from a
// new direction. Nothing here weighs that, and the honest way to ship a
// ranking that cannot see it is to say so where the decision is presented.
//
// Fails when no provider answered with anything. That is deliberately distinct
// from every provider answering with nothing: the first means we do not know,
// the second means the market does not have it.
func (s *Service) SelectCheapest(ctx context.Context, providerNames []string, reqs *provisionerv1.ResourceRequirements) (*Placement, error) {
	answers := s.ListCandidatesAcross(ctx, providerNames, reqs)
	ranked := MergeCandidates(answers)

	if len(ranked) == 0 {
		return nil, noPlacementError(answers)
	}

	runners := ranked[1:]
	if len(runners) > maxRunners {
		runners = runners[:maxRunners]
	}
	return &Placement{
		Winner:        ranked[0],
		Runners:       runners,
		Considered:    int32(len(ranked)),
		Answers:       answers,
		Comparability: AnalyzeComparability(answers),
	}, nil
}

// noPlacementError distinguishes the two ways a cross-provider search comes
// back empty, because they send an operator to different places.
//
// Every provider answering with nothing is a fact about the market: the
// requirement is real and nobody has the hardware right now, so widening the
// requirement or waiting are the moves. Nobody being able to answer is a fact
// about our integrations, and widening the requirement would not help.
func noPlacementError(answers []*ProviderAnswer) error {
	var answered, cannot, failed int
	for _, a := range answers {
		switch a.GetOutcome() {
		case provisionerv1.AnswerOutcome_ANSWER_OUTCOME_ANSWERED,
			provisionerv1.AnswerOutcome_ANSWER_OUTCOME_NO_CAPACITY:
			answered++
		case provisionerv1.AnswerOutcome_ANSWER_OUTCOME_CANNOT_ANSWER:
			cannot++
		default:
			failed++
		}
	}
	switch {
	case answered > 0:
		return status.Errorf(codes.ResourceExhausted,
			"no capacity: %d provider(s) answered and none had anything matching these requirements", answered)
	case failed > 0:
		return status.Errorf(codes.Unavailable,
			"could not place: %d provider(s) failed to answer and %d cannot answer at all; nobody looked", failed, cannot)
	default:
		return status.Errorf(codes.Unimplemented,
			"could not place: none of the %d provider(s) asked can list candidates without renting one", cannot)
	}
}

// DescribePlacement renders the reasoning behind a placement as operator-facing
// lines.
//
// A free function rather than a method, because Placement is now the generated
// message and Go cannot attach methods to an alias of a type from another
// package.
//
// Lives here rather than in the CLI because the same explanation is owed
// wherever a placement is made, and a decision explained one way by --dry-run
// and another way by the deploy log is a decision nobody trusts.
func DescribePlacement(p *Placement) []string {
	out := []string{
		fmt.Sprintf("cheapest of %d candidate(s): %s on %s at $%.2f/hr",
			p.GetConsidered(), p.GetWinner().GetSku(), p.GetWinner().GetProvider(),
			p.GetWinner().GetPriceUsdPerHour()),
	}
	for _, r := range p.GetRunners() {
		delta := r.GetPriceUsdPerHour() - p.GetWinner().GetPriceUsdPerHour()
		out = append(out, fmt.Sprintf("  next: %s on %s at $%.2f/hr (+$%.2f)",
			r.GetSku(), r.GetProvider(), r.GetPriceUsdPerHour(), delta))
	}
	// Only vendors that could have contributed and did not. A provider with
	// no way to answer is a standing fact about our integrations, and
	// repeating it on every placement would bury the case that matters: a
	// vendor that normally competes was unreachable this time, so the winner
	// won a smaller race than it looks.
	for _, a := range p.GetAnswers() {
		if a.GetOutcome() == provisionerv1.AnswerOutcome_ANSWER_OUTCOME_FAILED {
			out = append(out, fmt.Sprintf("  note: %s could not be reached, so it did not compete", a.GetProvider()))
		}
	}
	for _, g := range p.GetComparability().GetGaps() {
		out = append(out, fmt.Sprintf("  note: %s was not comparable across all providers, and was not ranked on", g.GetFact()))
	}
	return out
}
