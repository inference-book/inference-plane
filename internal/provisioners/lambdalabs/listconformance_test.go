package lambdalabs

import (
	"net/http"
	"testing"

	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/providertest"
)

// Lambda recovers iplane-id from the instance name and nothing else, since
// ownership rides on `name` rather than on Lambda's tags array. It used to
// read name-prefix and drop every other key.
func TestListFilterConformance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, instanceListResponse{Data: []apiInstance{
			{ID: "1", Name: "iplane-foo", Status: "active"},
			{ID: "2", Name: "iplane-bar", Status: "booting"},
		}})
	})
	p, _ := newTestProvider(t, mux)

	providertest.RunListFilterConformance(t, p, []providertest.SeededInstance{
		{ProviderID: "1", Tags: map[string]string{provisioners.TagID: "foo"}},
		{ProviderID: "2", Tags: map[string]string{provisioners.TagID: "bar"}},
	})
}

// A box launched from the Lambda console carries no iplane id.
func TestListStampsNoIDOnAForeignInstance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, instanceListResponse{Data: []apiInstance{
			{ID: "9", Name: "my-notebook-box", Status: "active"},
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
