package provisioners

import (
	"context"
	"sort"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// ProviderAnswer is one provider's response to a capacity question, kept whole
// rather than folded into a combined list.
//
// The three ways a provider can contribute nothing are different answers and
// an operator acts on each differently. Cannot-answer means look elsewhere for
// the information. Errored means try again or check a credential. Answered
// with nothing means this vendor genuinely has no capacity, which is the only
// one of the three that says anything about the market. Collapsing them into
// an empty list is the mistake ListCandidates already refuses to make for one
// provider, and it gets easier to make once several are in play.
type ProviderAnswer struct {
	Provider   string
	Candidates []Candidate

	// Err is non-nil when this provider did not answer. Unimplemented means
	// the provider has no way to answer at all; anything else is a failure of
	// this particular attempt.
	Err error
}

// CanAnswer reports whether the provider has the capability, regardless of
// whether this attempt succeeded.
func (a ProviderAnswer) CanAnswer() bool {
	return status.Code(a.Err) != codes.Unimplemented
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
func (s *Service) ListCandidatesAcross(ctx context.Context, providerNames []string, reqs *provisionerv1.ResourceRequirements) []ProviderAnswer {
	if len(providerNames) == 0 {
		for name := range s.providers {
			providerNames = append(providerNames, name)
		}
		sort.Strings(providerNames)
	}

	answers := make([]ProviderAnswer, len(providerNames))
	var wg sync.WaitGroup
	for i, name := range providerNames {
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			cands, err := s.ListCandidates(ctx, name, reqs)
			answers[i] = ProviderAnswer{Provider: name, Candidates: cands, Err: err}
		}(i, name)
	}
	wg.Wait()
	return answers
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
func MergeCandidates(answers []ProviderAnswer) []Candidate {
	var out []Candidate
	for _, a := range answers {
		out = append(out, a.Candidates...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].PriceUSDPerHour < out[j].PriceUSDPerHour
	})
	return out
}

// FactGap records one fact that some answering providers reported and others
// did not, which is the common case rather than the edge case.
type FactGap struct {
	Fact        string
	ReportedBy  []string
	MissingFrom []string
}

// Comparability describes what a merged list can honestly be compared on.
//
// It exists because the failure it prevents is invisible. Put three vendors'
// candidates in one table and the eye ranks them; a row with an empty column
// reads as neutral rather than as unknown, and a vendor that publishes less
// therefore looks no worse than one that publishes more. That is the ranking
// form of the rule the fabric package already enforces for a single
// requirement: silence is absence of information, never evidence.
//
// This does not filter or reweight anything. It reports, so the operator doing
// the comparing knows which columns mean the same thing on every row.
type Comparability struct {
	// Compared lists facts every answering provider populated on at least one
	// candidate, so a column-by-column reading of those is sound.
	Compared []string

	// Gaps lists facts reported unevenly. A candidate missing one of these is
	// not worse or better on it, it is unmeasured, and nothing should be
	// concluded from the blank.
	Gaps []FactGap
}

// comparabilityFacts are the typed fields worth telling an operator about.
//
// Attrs is deliberately absent. Its keys are each provider's own, so it is
// uncomparable by construction rather than by accident, and reporting every
// provider-specific key as a "gap" would bury the facts that genuinely could
// have been compared and were not.
var comparabilityFacts = []struct {
	name     string
	reported func(Candidate) bool
}{
	{"region", func(c Candidate) bool { return c.Region != "" }},
	{"host identity", func(c Candidate) bool { return c.HostID != "" }},
	{"architecture", func(c Candidate) bool { return c.Architecture != "" }},
	{"measured fabric", func(c Candidate) bool {
		return c.Fabric.Source == provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED
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
func AnalyzeComparability(answers []ProviderAnswer) Comparability {
	var participating []ProviderAnswer
	for _, a := range answers {
		if a.Err == nil && len(a.Candidates) > 0 {
			participating = append(participating, a)
		}
	}
	// Nothing to compare with fewer than two participants.
	if len(participating) < 2 {
		return Comparability{}
	}

	var comp Comparability
	for _, f := range comparabilityFacts {
		var reportedBy, missingFrom []string
		for _, a := range participating {
			if anyCandidateReports(a.Candidates, f.reported) {
				reportedBy = append(reportedBy, a.Provider)
			} else {
				missingFrom = append(missingFrom, a.Provider)
			}
		}
		switch {
		case len(missingFrom) == 0:
			comp.Compared = append(comp.Compared, f.name)
		case len(reportedBy) > 0:
			comp.Gaps = append(comp.Gaps, FactGap{
				Fact: f.name, ReportedBy: reportedBy, MissingFrom: missingFrom,
			})
		}
		// A fact nobody reported is not a gap. There is nothing to be
		// misled by when the column is empty for everyone.
	}
	return comp
}

func anyCandidateReports(cands []Candidate, reported func(Candidate) bool) bool {
	for _, c := range cands {
		if reported(c) {
			return true
		}
	}
	return false
}
