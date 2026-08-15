package provisioners_test

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
)

// stubSource is a Provider that also answers capacity questions, so a test can
// hold several vendors with different reporting habits without reaching for
// three real adapters.
type stubSource struct {
	provisioners.Provider
	name  string
	cands []provisioners.Candidate
	err   error
}

func (s *stubSource) Name() string { return s.name }

func (s *stubSource) Candidates(_ context.Context, _ *provisionerv1.ResourceRequirements) ([]provisioners.Candidate, error) {
	return s.cands, s.err
}

func serviceWith(t *testing.T, providers ...provisioners.Provider) *provisioners.Service {
	t.Helper()
	store, err := file.Open(t.TempDir(), "test-operator")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return provisioners.New(providers, store, "test-operator")
}

func cand(price float64) provisioners.Candidate {
	return provisioners.Candidate{PriceUSDPerHour: price}
}

// A search whose whole premise is that one vendor may not be able to supply
// what you need must not be defeated by one vendor being unreachable.
func TestOneProviderFailingDoesNotFailTheRest(t *testing.T) {
	svc := serviceWith(t,
		&stubSource{Provider: local.New(), name: "good", cands: []provisioners.Candidate{cand(1.0)}},
		&stubSource{Provider: local.New(), name: "broken", err: errors.New("connection refused")},
	)

	answers := svc.ListCandidatesAcross(context.Background(),
		[]string{"good", "broken"}, &provisionerv1.ResourceRequirements{})

	if len(answers) != 2 {
		t.Fatalf("got %d answers, want one per provider asked", len(answers))
	}
	if answers[0].Err != nil {
		t.Errorf("the working provider was penalised for its neighbour: %v", answers[0].Err)
	}
	if len(answers[0].Candidates) != 1 {
		t.Errorf("lost the working provider's candidates: %+v", answers[0])
	}
	if answers[1].Err == nil {
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
		if answers[i].Provider != w {
			t.Fatalf("answer order = %v..., want %v", answers[i].Provider, want)
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
		if !a.CanAnswer() {
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

	if !answers[0].CanAnswer() {
		t.Error("a transport failure was reported as a missing capability")
	}
	if answers[1].CanAnswer() {
		t.Error("a provider with no capability was reported as merely failing")
	}
	if status.Code(answers[1].Err) != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", status.Code(answers[1].Err))
	}
}

// The merged list is ranked across vendors, not concatenated per vendor, and
// ties hold the order asked so two vendors quoting the same number do not swap
// places between runs.
func TestMergeRanksAcrossProvidersAndHoldsTies(t *testing.T) {
	answers := []provisioners.ProviderAnswer{
		{Provider: "a", Candidates: []provisioners.Candidate{
			{Provider: "a", PriceUSDPerHour: 3.0}, {Provider: "a", PriceUSDPerHour: 1.0}}},
		{Provider: "b", Candidates: []provisioners.Candidate{
			{Provider: "b", PriceUSDPerHour: 2.0}, {Provider: "b", PriceUSDPerHour: 1.0}}},
	}

	got := provisioners.MergeCandidates(answers)

	wantPrices := []float64{1.0, 1.0, 2.0, 3.0}
	if len(got) != len(wantPrices) {
		t.Fatalf("got %d candidates, want %d", len(got), len(wantPrices))
	}
	for i, w := range wantPrices {
		if got[i].PriceUSDPerHour != w {
			t.Fatalf("prices = %v, want %v", pricesOf(got), wantPrices)
		}
	}
	// The tie resolves to the provider asked first. Worth stating, but note
	// this assertion cannot actually detect an unstable sort: Go's pdqsort
	// short-circuits runs of equal keys, so sort.Slice and sort.SliceStable
	// produce the same order here and at 80 elements too. Stability comes from
	// the SliceStable call in MergeCandidates, not from this line.
	if got[0].Provider != "a" {
		t.Errorf("tie resolved to %q, want the provider asked first", got[0].Provider)
	}
}

func pricesOf(cs []provisioners.Candidate) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.PriceUSDPerHour
	}
	return out
}

// The failure comparability exists to prevent is invisible: in a merged table a
// blank cell reads as neutral, so the vendor publishing least looks no worse
// than the one publishing most. Naming the uneven facts is the whole job.
func TestComparabilitySeparatesSharedFactsFromGaps(t *testing.T) {
	answers := []provisioners.ProviderAnswer{
		{Provider: "rich", Candidates: []provisioners.Candidate{{
			Provider: "rich", Region: "eu-1", HostID: "h1", Architecture: "amd64",
			Fabric: fabric.Result{Source: provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED},
		}}},
		{Provider: "sparse", Candidates: []provisioners.Candidate{{
			Provider: "sparse", Architecture: "amd64",
			Fabric: fabric.Result{Source: provisionerv1.FabricSource_FABRIC_SOURCE_DECLARED},
		}}},
	}

	got := provisioners.AnalyzeComparability(answers)

	if len(got.Compared) != 1 || got.Compared[0] != "architecture" {
		t.Errorf("compared = %v, want just architecture", got.Compared)
	}
	gaps := map[string]bool{}
	for _, g := range got.Gaps {
		gaps[g.Fact] = true
		if len(g.ReportedBy) != 1 || g.ReportedBy[0] != "rich" {
			t.Errorf("gap %q reportedBy = %v, want [rich]", g.Fact, g.ReportedBy)
		}
		if len(g.MissingFrom) != 1 || g.MissingFrom[0] != "sparse" {
			t.Errorf("gap %q missingFrom = %v, want [sparse]", g.Fact, g.MissingFrom)
		}
	}
	for _, want := range []string{"region", "host identity", "measured fabric"} {
		if !gaps[want] {
			t.Errorf("%q was not reported as a gap; gaps = %v", want, got.Gaps)
		}
	}
}

// A fact nobody reports is not a gap. There is nothing to be misled by when the
// column is blank on every row, and calling it a gap would bury the facts that
// genuinely could have been compared and were not.
func TestComparabilityIgnoresFactsNobodyReports(t *testing.T) {
	answers := []provisioners.ProviderAnswer{
		{Provider: "a", Candidates: []provisioners.Candidate{{Provider: "a", Architecture: "amd64"}}},
		{Provider: "b", Candidates: []provisioners.Candidate{{Provider: "b", Architecture: "arm64"}}},
	}

	got := provisioners.AnalyzeComparability(answers)

	for _, g := range got.Gaps {
		if g.Fact == "region" || g.Fact == "host identity" {
			t.Errorf("%q reported as a gap though neither provider publishes it", g.Fact)
		}
	}
}

// Providers that could not answer are absent from the comparison rather than
// weak in it. Counting them as missing every fact would drown the real gaps.
func TestComparabilitySkipsProvidersThatDidNotAnswer(t *testing.T) {
	answers := []provisioners.ProviderAnswer{
		{Provider: "a", Candidates: []provisioners.Candidate{{Provider: "a", Region: "eu-1", Architecture: "amd64"}}},
		{Provider: "b", Candidates: []provisioners.Candidate{{Provider: "b", Region: "us-1", Architecture: "arm64"}}},
		{Provider: "cannot", Err: status.Error(codes.Unimplemented, "no")},
	}

	got := provisioners.AnalyzeComparability(answers)

	if len(got.Gaps) != 0 {
		t.Errorf("gaps = %+v, want none: the two answering providers report the same facts", got.Gaps)
	}
}

// With one participant there is no comparison to characterise, and printing a
// list of "comparable" facts would imply one had happened.
func TestComparabilityIsEmptyBelowTwoParticipants(t *testing.T) {
	answers := []provisioners.ProviderAnswer{
		{Provider: "only", Candidates: []provisioners.Candidate{{Provider: "only", Region: "eu-1"}}},
	}

	got := provisioners.AnalyzeComparability(answers)

	if len(got.Compared) != 0 || len(got.Gaps) != 0 {
		t.Errorf("got %+v, want an empty report for a single provider", got)
	}
}
