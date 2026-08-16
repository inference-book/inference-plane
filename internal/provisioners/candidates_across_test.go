package provisioners_test

import (
	"context"
	"errors"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// stubSource is a Provider that also answers capacity questions, so a test can
// hold several vendors with different reporting habits without reaching for
// three real adapters.
type stubSource struct {
	provisioners.Provider
	name  string
	cands []*provisioners.Candidate
	err   error
}

func (s *stubSource) Name() string { return s.name }

func (s *stubSource) Candidates(_ context.Context, _ *provisionerv1.ResourceRequirements) ([]*provisioners.Candidate, error) {
	return s.cands, s.err
}

const answered = provisionerv1.AnswerOutcome_ANSWER_OUTCOME_ANSWERED

func serviceWith(t *testing.T, providers ...provisioners.Provider) *provisioners.Service {
	t.Helper()
	store, err := file.Open(t.TempDir(), "test-operator")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return provisioners.New(providers, store, "test-operator")
}

func cand(price float64) *provisioners.Candidate {
	return &provisioners.Candidate{PriceUsdPerHour: price}
}

// A search whose whole premise is that one vendor may not be able to supply
// what you need must not be defeated by one vendor being unreachable.
func TestOneProviderFailingDoesNotFailTheRest(t *testing.T) {
	svc := serviceWith(t,
		&stubSource{Provider: local.New(), name: "good", cands: []*provisioners.Candidate{cand(1.0)}},
		&stubSource{Provider: local.New(), name: "broken", err: errors.New("connection refused")},
	)

	answers := svc.ListCandidatesAcross(context.Background(),
		[]string{"good", "broken"}, &provisionerv1.ResourceRequirements{})

	if len(answers) != 2 {
		t.Fatalf("got %d answers, want one per provider asked", len(answers))
	}
	if answers[0].GetOutcome() == provisionerv1.AnswerOutcome_ANSWER_OUTCOME_FAILED {
		t.Errorf("the working provider was penalised for its neighbour: %v", answers[0].GetError())
	}
	if len(answers[0].GetCandidates()) != 1 {
		t.Errorf("lost the working provider's candidates: %+v", answers[0])
	}
	if answers[1].GetOutcome() != provisionerv1.AnswerOutcome_ANSWER_OUTCOME_FAILED {
		t.Error("the broken provider reported success")
	}
}

// Answers come back in the order asked regardless of which provider replies
// first, so a run is reproducible and the summary does not reshuffle between
// invocations for reasons that are purely about network timing.
func TestAnswersHoldTheOrderAsked(t *testing.T) {
	svc := serviceWith(t,
		&stubSource{Provider: local.New(), name: "aaa"},
		&stubSource{Provider: local.New(), name: "bbb"},
		&stubSource{Provider: local.New(), name: "ccc"},
	)

	answers := svc.ListCandidatesAcross(context.Background(),
		[]string{"ccc", "aaa", "bbb"}, &provisionerv1.ResourceRequirements{})

	want := []string{"ccc", "aaa", "bbb"}
	for i, w := range want {
		if answers[i].GetProvider() != w {
			t.Fatalf("answer order = %v..., want %v", answers[i].GetProvider(), want)
		}
	}
}

// "Which of my vendors can even tell me this" is a question worth asking, so
// an empty list means every configured provider including the ones that cannot
// answer.
func TestEmptyProviderListAsksEveryone(t *testing.T) {
	svc := serviceWith(t,
		&stubSource{Provider: local.New(), name: "answers"},
		local.New(), // has no CandidateSource
	)

	answers := svc.ListCandidatesAcross(context.Background(), nil, &provisionerv1.ResourceRequirements{})

	if len(answers) != 2 {
		t.Fatalf("got %d answers, want every configured provider", len(answers))
	}
	var sawCannotAnswer bool
	for _, a := range answers {
		if !provisioners.CanAnswer(a) {
			sawCannotAnswer = true
		}
	}
	if !sawCannotAnswer {
		t.Error("a provider without the capability was not reported as unable to answer")
	}
}

// Cannot-answer and did-not-answer send an operator to different places: one
// to another source of information, the other to a credential or a retry.
func TestCannotAnswerIsDistinctFromFailed(t *testing.T) {
	svc := serviceWith(t,
		&stubSource{Provider: local.New(), name: "broken", err: errors.New("connection refused")},
		local.New(),
	)

	answers := svc.ListCandidatesAcross(context.Background(),
		[]string{"broken", "local"}, &provisionerv1.ResourceRequirements{})

	if !provisioners.CanAnswer(answers[0]) {
		t.Error("a transport failure was reported as a missing capability")
	}
	if provisioners.CanAnswer(answers[1]) {
		t.Error("a provider with no capability was reported as merely failing")
	}
	// The outcome is what travels now, so assert on that rather than on a
	// gRPC code a remote caller would never see.
	if answers[1].GetOutcome() != provisionerv1.AnswerOutcome_ANSWER_OUTCOME_CANNOT_ANSWER {
		t.Errorf("outcome = %v, want CANNOT_ANSWER", answers[1].GetOutcome())
	}
}

// The merged list is ranked across vendors, not concatenated per vendor, and
// ties hold the order asked so two vendors quoting the same number do not swap
// places between runs.
func TestMergeRanksAcrossProvidersAndHoldsTies(t *testing.T) {
	answers := []*provisioners.ProviderAnswer{
		&provisioners.ProviderAnswer{Outcome: answered, Provider: "a", Candidates: []*provisioners.Candidate{
			{Provider: "a", PriceUsdPerHour: 3.0}, {Provider: "a", PriceUsdPerHour: 1.0}}},
		&provisioners.ProviderAnswer{Outcome: answered, Provider: "b", Candidates: []*provisioners.Candidate{
			{Provider: "b", PriceUsdPerHour: 2.0}, {Provider: "b", PriceUsdPerHour: 1.0}}},
	}

	got := provisioners.MergeCandidates(answers)

	wantPrices := []float64{1.0, 1.0, 2.0, 3.0}
	if len(got) != len(wantPrices) {
		t.Fatalf("got %d candidates, want %d", len(got), len(wantPrices))
	}
	for i, w := range wantPrices {
		if got[i].GetPriceUsdPerHour() != w {
			t.Fatalf("prices = %v, want %v", pricesOf(got), wantPrices)
		}
	}
	// The tie resolves to the provider asked first. Worth stating, but note
	// this assertion cannot actually detect an unstable sort: Go's pdqsort
	// short-circuits runs of equal keys, so sort.Slice and sort.SliceStable
	// produce the same order here and at 80 elements too. Stability comes from
	// the SliceStable call in MergeCandidates, not from this line.
	if got[0].GetProvider() != "a" {
		t.Errorf("tie resolved to %q, want the provider asked first", got[0].GetProvider())
	}
}

func pricesOf(cs []*provisioners.Candidate) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.GetPriceUsdPerHour()
	}
	return out
}

// The failure comparability exists to prevent is invisible: in a merged table a
// blank cell reads as neutral, so the vendor publishing least looks no worse
// than the one publishing most. Naming the uneven facts is the whole job.
func TestComparabilitySeparatesSharedFactsFromGaps(t *testing.T) {
	answers := []*provisioners.ProviderAnswer{
		&provisioners.ProviderAnswer{Outcome: answered, Provider: "rich", Candidates: []*provisioners.Candidate{{
			Provider: "rich", Region: "eu-1", HostId: "h1", Architecture: "amd64",
			FabricSource: provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED,
		}}},
		&provisioners.ProviderAnswer{Outcome: answered, Provider: "sparse", Candidates: []*provisioners.Candidate{{
			Provider: "sparse", Architecture: "amd64",
			FabricSource: provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED,
		}}},
	}

	got := provisioners.AnalyzeComparability(answers)

	if len(got.GetCompared()) != 1 || got.GetCompared()[0] != "architecture" {
		t.Errorf("compared = %v, want just architecture", got.GetCompared())
	}
	gaps := map[string]bool{}
	for _, g := range got.GetGaps() {
		gaps[g.GetFact()] = true
		if len(g.GetReportedBy()) != 1 || g.GetReportedBy()[0] != "rich" {
			t.Errorf("gap %q reportedBy = %v, want [rich]", g.GetFact(), g.GetReportedBy())
		}
		if len(g.GetMissingFrom()) != 1 || g.GetMissingFrom()[0] != "sparse" {
			t.Errorf("gap %q missingFrom = %v, want [sparse]", g.GetFact(), g.GetMissingFrom())
		}
	}
	for _, want := range []string{"region", "host identity", "measured fabric"} {
		if !gaps[want] {
			t.Errorf("%q was not reported as a gap; gaps = %v", want, got.GetGaps())
		}
	}
}

// A fact nobody reports is not a gap. There is nothing to be misled by when the
// column is blank on every row, and calling it a gap would bury the facts that
// genuinely could have been compared and were not.
func TestComparabilityIgnoresFactsNobodyReports(t *testing.T) {
	answers := []*provisioners.ProviderAnswer{
		&provisioners.ProviderAnswer{Outcome: answered, Provider: "a", Candidates: []*provisioners.Candidate{{Provider: "a", Architecture: "amd64"}}},
		&provisioners.ProviderAnswer{Outcome: answered, Provider: "b", Candidates: []*provisioners.Candidate{{Provider: "b", Architecture: "arm64"}}},
	}

	got := provisioners.AnalyzeComparability(answers)

	for _, g := range got.GetGaps() {
		if g.GetFact() == "region" || g.GetFact() == "host identity" {
			t.Errorf("%q reported as a gap though neither provider publishes it", g.GetFact())
		}
	}
}

// Providers that could not answer are absent from the comparison rather than
// weak in it. Counting them as missing every fact would drown the real gaps.
func TestComparabilitySkipsProvidersThatDidNotAnswer(t *testing.T) {
	answers := []*provisioners.ProviderAnswer{
		&provisioners.ProviderAnswer{Outcome: answered, Provider: "a", Candidates: []*provisioners.Candidate{{Provider: "a", Region: "eu-1", Architecture: "amd64"}}},
		&provisioners.ProviderAnswer{Outcome: answered, Provider: "b", Candidates: []*provisioners.Candidate{{Provider: "b", Region: "us-1", Architecture: "arm64"}}},
		&provisioners.ProviderAnswer{Outcome: provisionerv1.AnswerOutcome_ANSWER_OUTCOME_CANNOT_ANSWER, Provider: "cannot"},
	}

	got := provisioners.AnalyzeComparability(answers)

	if len(got.GetGaps()) != 0 {
		t.Errorf("gaps = %+v, want none: the two answering providers report the same facts", got.GetGaps())
	}
}

// With one participant there is no comparison to characterise, and printing a
// list of "comparable" facts would imply one had happened.
func TestComparabilityIsEmptyBelowTwoParticipants(t *testing.T) {
	answers := []*provisioners.ProviderAnswer{
		&provisioners.ProviderAnswer{Outcome: answered, Provider: "only", Candidates: []*provisioners.Candidate{{Provider: "only", Region: "eu-1"}}},
	}

	got := provisioners.AnalyzeComparability(answers)

	if len(got.GetCompared()) != 0 || len(got.GetGaps()) != 0 {
		t.Errorf("got %+v, want an empty report for a single provider", got)
	}
}
