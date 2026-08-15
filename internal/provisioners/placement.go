package provisioners

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Placement is a decision about where to put something, together with enough
// of the reasoning to argue with.
//
// Runners carries the candidates that lost, because a placement an operator
// cannot interrogate is one they have to take on trust, and "why did it pick
// that" is the first question anyone asks of an automatic choice. It is also
// what makes a dry-run worth reading.
type Placement struct {
	// Winner is the candidate chosen.
	Winner Candidate

	// Runners are the next few that lost, cheapest first, for the same
	// requirement. Not the whole set: enough to see whether the decision was
	// close.
	Runners []Candidate

	// Considered counts every candidate across every answering provider, so
	// the operator can tell a decision made from three options from one made
	// from three hundred.
	Considered int

	// Answers is the per-provider outcome behind the decision, so a placement
	// made while one vendor was unreachable says so rather than looking like a
	// survey of the whole market.
	Answers []ProviderAnswer

	// Comparability describes what the ranking could honestly compare. A
	// cheapest-fit decision taken across vendors that report different facts
	// is still the cheapest, and it is not necessarily the best, and the
	// difference belongs in front of whoever is about to spend the money.
	Comparability Comparability
}

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
		Considered:    len(ranked),
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
func noPlacementError(answers []ProviderAnswer) error {
	var answered, cannot, failed int
	for _, a := range answers {
		switch {
		case a.Err == nil:
			answered++
		case !a.CanAnswer():
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

// Describe renders the reasoning behind a placement as operator-facing lines.
//
// Lives here rather than in the CLI because the same explanation is owed
// wherever a placement is made, and a decision explained one way by --dry-run
// and another way by the deploy log is a decision nobody trusts.
func (p *Placement) Describe() []string {
	out := []string{
		fmt.Sprintf("cheapest of %d candidate(s): %s on %s at $%.2f/hr",
			p.Considered, p.Winner.SKU, p.Winner.Provider, p.Winner.PriceUSDPerHour),
	}
	for _, r := range p.Runners {
		delta := r.PriceUSDPerHour - p.Winner.PriceUSDPerHour
		out = append(out, fmt.Sprintf("  next: %s on %s at $%.2f/hr (+$%.2f)",
			r.SKU, r.Provider, r.PriceUSDPerHour, delta))
	}
	// Only vendors that could have contributed and did not. A provider with
	// no way to answer is a standing fact about our integrations, and
	// repeating it on every placement would bury the case that matters: a
	// vendor that normally competes was unreachable this time, so the winner
	// won a smaller race than it looks.
	for _, a := range p.Answers {
		if a.Err != nil && a.CanAnswer() {
			out = append(out, fmt.Sprintf("  note: %s could not be reached, so it did not compete", a.Provider))
		}
	}
	for _, g := range p.Comparability.Gaps {
		out = append(out, fmt.Sprintf("  note: %s was not comparable across all providers, and was not ranked on", g.Fact))
	}
	return out
}
