package provisioners

import (
	"context"
	"sort"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// ProviderAnswer and the rest are the generated messages. See Candidate in
// candidates.go for why these are aliases rather than parallel Go structs.
type (
	ProviderAnswer = provisionerv1.ProviderAnswer
	Comparability  = provisionerv1.Comparability
	FactGap        = provisionerv1.FactGap
)

// CanAnswer reports whether the provider has the capability at all, regardless
// of whether this attempt succeeded.
//
// A free function rather than a method because Go cannot attach methods to an
// alias of a type from another package. The distinction it draws is the one the
// whole capacity surface rests on: a provider with no way to answer sends the
// operator to another source of information, while one that merely failed
// sends them to a retry or a credential.
func CanAnswer(a *ProviderAnswer) bool {
	return a.GetOutcome() != provisionerv1.AnswerOutcome_ANSWER_OUTCOME_CANNOT_ANSWER
}

// ListCandidatesAcross asks several providers the same question at once and
// returns each answer separately, in the order asked.
//
// Passing no names asks every configured provider, including the ones that
// cannot answer, because "which of my vendors can even tell me this" is a
// question worth being able to ask.
//
// One provider failing never fails the call. A cross-provider search whose
// whole point is that one vendor may not be able to supply what you need
// should not be defeated by one vendor being unreachable.
func (s *Service) ListCandidatesAcross(ctx context.Context, providerNames []string, reqs *provisionerv1.ResourceRequirements) []*ProviderAnswer {
	if len(providerNames) == 0 {
		for name := range s.providers {
			providerNames = append(providerNames, name)
		}
		sort.Strings(providerNames)
	}

	answers := make([]*ProviderAnswer, len(providerNames))
	var wg sync.WaitGroup
	for i, name := range providerNames {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			cands, err := s.ListCandidates(ctx, name, reqs)
			answers[i] = answerFor(name, cands, err)
		}(i, name)
	}
	wg.Wait()
	return answers
}

// answerFor classifies one provider's response into the four outcomes.
//
// Classifying here rather than in the renderer keeps "answered with nothing"
// and "cannot answer" apart on the wire, which is where they need to stay: a
// remote caller reconstructing the difference from an empty list and an error
// string would get it wrong in exactly the case the distinction exists for.
func answerFor(name string, cands []*Candidate, err error) *ProviderAnswer {
	out := &ProviderAnswer{Provider: name, Candidates: cands}
	switch {
	case err != nil && status.Code(err) == codes.Unimplemented:
		out.Outcome = provisionerv1.AnswerOutcome_ANSWER_OUTCOME_CANNOT_ANSWER
		out.Error = err.Error()
	case err != nil:
		out.Outcome = provisionerv1.AnswerOutcome_ANSWER_OUTCOME_FAILED
		out.Error = err.Error()
	case len(cands) == 0:
		out.Outcome = provisionerv1.AnswerOutcome_ANSWER_OUTCOME_NO_CAPACITY
	default:
		out.Outcome = provisionerv1.AnswerOutcome_ANSWER_OUTCOME_ANSWERED
	}
	return out
}

// MergeCandidates flattens the answers into one list, cheapest first.
//
// Price is the only ranking key, and that is a deliberate limit rather than a
// finished design. Ranking on anything else means deciding that a fact from
// one vendor is commensurable with a fact from another, and the whole reason
// Comparability exists is that it usually is not. Ranking on more than price
// is tracked separately.
//
// Ties hold the order providers were asked in, so a run is reproducible and
// two vendors quoting the same number do not swap places between invocations.
func MergeCandidates(answers []*ProviderAnswer) []*Candidate {
	var out []*Candidate
	for _, a := range answers {
		out = append(out, a.GetCandidates()...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].GetPriceUsdPerHour() < out[j].GetPriceUsdPerHour()
	})
	return out
}

// comparabilityFacts are the typed fields worth telling an operator about.
//
// Attrs is deliberately absent. Its keys are each provider's own, so it is
// uncomparable by construction rather than by accident, and reporting every
// provider-specific key as a "gap" would bury the facts that genuinely could
// have been compared and were not.
var comparabilityFacts = []struct {
	name     string
	reported func(*Candidate) bool
}{
	{"region", func(c *Candidate) bool { return c.GetRegion() != "" }},
	{"host identity", func(c *Candidate) bool { return c.GetHostId() != "" }},
	{"architecture", func(c *Candidate) bool { return c.GetArchitecture() != "" }},
	{"measured fabric", func(c *Candidate) bool {
		return c.GetFabricSource() == provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED
	}},
}

// AnalyzeComparability works out which facts the answering providers agree on
// reporting, from the candidates actually returned rather than from a table of
// what each provider is supposed to support.
//
// Deriving it from the data matters. A provider that normally reports a fact
// but returned nothing carrying it this time really has left a gap in THIS
// comparison, and a static capability table would claim otherwise.
//
// Providers that could not answer are skipped entirely. They are absent from
// the comparison rather than weak in it, which the caller reports separately.
func AnalyzeComparability(answers []*ProviderAnswer) *Comparability {
	var participating []*ProviderAnswer
	for _, a := range answers {
		if a.GetOutcome() == provisionerv1.AnswerOutcome_ANSWER_OUTCOME_ANSWERED {
			participating = append(participating, a)
		}
	}
	// Nothing to compare with fewer than two participants.
	if len(participating) < 2 {
		return &Comparability{}
	}

	comp := &Comparability{}
	for _, f := range comparabilityFacts {
		var reportedBy, missingFrom []string
		for _, a := range participating {
			if anyCandidateReports(a.GetCandidates(), f.reported) {
				reportedBy = append(reportedBy, a.Provider)
			} else {
				missingFrom = append(missingFrom, a.Provider)
			}
		}
		switch {
		case len(missingFrom) == 0:
			comp.Compared = append(comp.Compared, f.name)
		case len(reportedBy) > 0:
			comp.Gaps = append(comp.Gaps, &FactGap{
				Fact: f.name, ReportedBy: reportedBy, MissingFrom: missingFrom,
			})
		}
		// A fact nobody reported is not a gap. There is nothing to be
		// misled by when the column is empty for everyone.
	}
	return comp
}

func anyCandidateReports(cands []*Candidate, reported func(*Candidate) bool) bool {
	for _, c := range cands {
		if reported(c) {
			return true
		}
	}
	return false
}
