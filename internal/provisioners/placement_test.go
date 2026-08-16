package provisioners_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
)

func priced(provider, sku string, price float64) *provisioners.Candidate {
	return &provisioners.Candidate{Provider: provider, Sku: sku, PriceUsdPerHour: price}
}

// The point of Act 3: the cheapest wins across vendors, not the first vendor
// asked. A per-provider search picks whoever answers first, which is an
// accident of ordering rather than a decision.
func TestSelectCheapestPicksAcrossProviders(t *testing.T) {
	svc := serviceWith(t,
		&stubSource{Provider: local.New(), name: "dear", cands: []*provisioners.Candidate{priced("dear", "a", 5.00)}},
		&stubSource{Provider: local.New(), name: "cheap", cands: []*provisioners.Candidate{priced("cheap", "b", 1.00)}},
	)

	// "dear" is asked first on purpose.
	got, err := svc.SelectCheapest(context.Background(),
		[]string{"dear", "cheap"}, &provisionerv1.ResourceRequirements{})
	if err != nil {
		t.Fatalf("SelectCheapest: %v", err)
	}

	if got.GetWinner().GetProvider() != "cheap" {
		t.Errorf("winner came from %q, want the cheaper vendor regardless of ask order", got.GetWinner().GetProvider())
	}
	if got.GetConsidered() != 2 {
		t.Errorf("considered = %d, want 2", got.GetConsidered())
	}
}

// A placement an operator cannot interrogate is one they take on trust, and
// "why that one" is the first question anyone asks of an automatic choice.
func TestPlacementCarriesItsRunnersUp(t *testing.T) {
	svc := serviceWith(t, &stubSource{Provider: local.New(), name: "p", cands: []*provisioners.Candidate{
		priced("p", "cheapest", 1.00), priced("p", "second", 1.50), priced("p", "third", 2.00),
	}})

	got, err := svc.SelectCheapest(context.Background(), []string{"p"}, &provisionerv1.ResourceRequirements{})
	if err != nil {
		t.Fatalf("SelectCheapest: %v", err)
	}

	if len(got.GetRunners()) != 2 {
		t.Fatalf("runners = %d, want the two that lost", len(got.GetRunners()))
	}
	desc := strings.Join(provisioners.DescribePlacement(got), "\n")
	if !strings.Contains(desc, "cheapest") || !strings.Contains(desc, "second") {
		t.Errorf("description does not let an operator see the alternatives:\n%s", desc)
	}
	if !strings.Contains(desc, "+$0.50") {
		t.Errorf("description does not show how close the decision was:\n%s", desc)
	}
}

// "Nobody has this" and "nobody looked" are different findings. The first says
// widen the requirement or wait; the second says the requirement was never
// tested and widening it would not help.
func TestNoCapacityIsDistinctFromNobodyLooked(t *testing.T) {
	answered := serviceWith(t, &stubSource{Provider: local.New(), name: "empty"})
	_, err := answered.SelectCheapest(context.Background(), []string{"empty"}, &provisionerv1.ResourceRequirements{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Errorf("a provider that answered with nothing gave %v, want ResourceExhausted", status.Code(err))
	}

	broken := serviceWith(t, &stubSource{Provider: local.New(), name: "broken", err: errors.New("refused")})
	_, err = broken.SelectCheapest(context.Background(), []string{"broken"}, &provisionerv1.ResourceRequirements{})
	if status.Code(err) != codes.Unavailable {
		t.Errorf("a provider that failed gave %v, want Unavailable", status.Code(err))
	}

	mute := serviceWith(t, local.New())
	_, err = mute.SelectCheapest(context.Background(), []string{"local"}, &provisionerv1.ResourceRequirements{})
	if status.Code(err) != codes.Unimplemented {
		t.Errorf("a provider with no capability gave %v, want Unimplemented", status.Code(err))
	}
}

// A vendor that normally competes being unreachable means the winner won a
// smaller race than it looks, and whoever is about to spend money should be
// told. A vendor that structurally cannot answer is a standing fact and would
// only be noise repeated on every placement.
func TestDescribeNotesUnreachableVendorsButNotMuteOnes(t *testing.T) {
	svc := serviceWith(t,
		&stubSource{Provider: local.New(), name: "ok", cands: []*provisioners.Candidate{priced("ok", "a", 1.00)}},
		&stubSource{Provider: local.New(), name: "flaky", err: errors.New("connection refused")},
		local.New(),
	)

	got, err := svc.SelectCheapest(context.Background(),
		[]string{"ok", "flaky", "local"}, &provisionerv1.ResourceRequirements{})
	if err != nil {
		t.Fatalf("SelectCheapest: %v", err)
	}

	desc := strings.Join(provisioners.DescribePlacement(got), "\n")
	if !strings.Contains(desc, "flaky") {
		t.Errorf("an unreachable competitor was not disclosed:\n%s", desc)
	}
	if strings.Contains(desc, "local") {
		t.Errorf("a provider that structurally cannot answer was reported as a missed competitor:\n%s", desc)
	}
}

// Cheapest is not best, and the ranking cannot see the facts it did not
// compare. Saying so at the point of decision is the difference between a
// ranking and a recommendation.
func TestDescribeDisclosesWhatItCouldNotCompare(t *testing.T) {
	svc := serviceWith(t,
		&stubSource{Provider: local.New(), name: "rich", cands: []*provisioners.Candidate{
			&provisioners.Candidate{Provider: "rich", Sku: "a", PriceUsdPerHour: 1.00, Region: "eu-1"}}},
		&stubSource{Provider: local.New(), name: "sparse", cands: []*provisioners.Candidate{
			&provisioners.Candidate{Provider: "sparse", Sku: "b", PriceUsdPerHour: 2.00}}},
	)

	got, err := svc.SelectCheapest(context.Background(),
		[]string{"rich", "sparse"}, &provisionerv1.ResourceRequirements{})
	if err != nil {
		t.Fatalf("SelectCheapest: %v", err)
	}

	desc := strings.Join(provisioners.DescribePlacement(got), "\n")
	if !strings.Contains(desc, "region") || !strings.Contains(desc, "not ranked on") {
		t.Errorf("the decision did not disclose the fact it could not compare:\n%s", desc)
	}
}
