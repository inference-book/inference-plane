package vast

import (
	"net/http"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/providertest"
)

// Vast recovers iplane-id from the instance label and nothing else, so the
// shared suite holds it to the contract over that tag. It used to read
// label-prefix and drop every other key, which meant a filter naming one
// instance came back with the whole account.
func TestListFilterConformance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, instanceListResponse{Instances: []apiInstance{
			{ID: 1, Label: "iplane-foo", ActualStatus: "running"},
			{ID: 2, Label: "iplane-bar", ActualStatus: "loading"},
		}})
	})
	p, _ := newTestProvider(t, mux)

	providertest.RunListFilterConformance(t, p, []providertest.SeededInstance{
		{ProviderID: "1", Tags: map[string]string{provisioners.TagID: "foo"}},
		{ProviderID: "2", Tags: map[string]string{provisioners.TagID: "bar"}},
	})
}

// A box the operator rented by hand carries no iplane id. TrimPrefix returns
// the label unchanged when the prefix is absent, so it used to come back
// wearing its own label as an iplane id, and the Service's ownership check
// reads exactly this tag.
func TestListStampsNoIDOnAForeignInstance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, instanceListResponse{Instances: []apiInstance{
			{ID: 9, Label: "research-cluster", ActualStatus: "running"},
		}})
	})
	p, _ := newTestProvider(t, mux)

	refs, err := p.List(t.Context(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("len(refs) = %d, want 1", len(refs))
	}
	if got := refs[0].GetTags()[provisioners.TagID]; got != "" {
		t.Errorf("foreign instance carries %s=%q, want no iplane id", provisioners.TagID, got)
	}
}
