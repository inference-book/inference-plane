package runpod

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// gpuTypesFixture mirrors the live payload shape, verified against the real
// GraphQL API on 2026-08-15. The nulls are the important part: RunPod sends
// them for a type nobody is offering at the requested width, and they are the
// availability signal rather than a malformed response.
const gpuTypesFixture = `{"gpuTypes":[
 {"id":"NVIDIA A100 80GB PCIe","displayName":"A100 PCIe","memoryInGb":80,
  "secureCloud":true,"communityCloud":true,
  "lowestPrice":{"uninterruptablePrice":1.19,"stockStatus":"Low"}},
 {"id":"NVIDIA A100-SXM4-80GB","displayName":"A100 SXM","memoryInGb":80,
  "secureCloud":true,"communityCloud":true,
  "lowestPrice":{"uninterruptablePrice":1.39,"stockStatus":"Medium"}},
 {"id":"NVIDIA H100 PCIe","displayName":"H100 PCIe","memoryInGb":80,
  "secureCloud":true,"communityCloud":true,
  "lowestPrice":{"uninterruptablePrice":null,"stockStatus":null}},
 {"id":"NVIDIA L4","displayName":"L4","memoryInGb":24,
  "secureCloud":true,"communityCloud":false,
  "lowestPrice":{"uninterruptablePrice":0.43,"stockStatus":"High"}},
 {"id":"NVIDIA H100 80GB HBM3","displayName":"H100 SXM","memoryInGb":80,
  "secureCloud":true,"communityCloud":true,
  "lowestPrice":{"uninterruptablePrice":2.69,"stockStatus":null}},
 {"id":"NVIDIA H100 NVL","displayName":"H100 NVL","memoryInGb":94,
  "secureCloud":true,"communityCloud":true,
  "lowestPrice":{"uninterruptablePrice":null,"stockStatus":"Low"}}
]}`

// candidateProvider serves the fixture for any gpuTypes query and records
// every query body, so a test can assert on what actually went over the wire.
func candidateProvider(t *testing.T, queries *[]string) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(body, &env)
		if queries != nil {
			*queries = append(*queries, env.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":` + gpuTypesFixture + `}`))
	}))
	t.Cleanup(srv.Close)
	rt := &rewriteTransport{toHost: strings.TrimPrefix(srv.URL, "http://")}
	return New(NewClient("test-api-key", WithHTTPClient(&http.Client{Transport: rt})))
}

// A null stock status means the type cannot be had at this width. It is the
// one thing RunPod publishes that a catalog cannot, so dropping it on the
// floor would leave this call buying nothing over --dry-run.
func TestCandidatesSkipUnavailableTypes(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 80})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	// Three ways to be unavailable, and each has to be caught on its own.
	// Both-null is the shape RunPod actually sends; the two half-null rows
	// exist so a check on one field cannot pass for a check on both.
	unavailable := map[string]string{
		"NVIDIA H100 PCIe":      "both fields null",
		"NVIDIA H100 80GB HBM3": "priced but no stock reported",
		"NVIDIA H100 NVL":       "stock reported but no price",
	}
	for _, c := range got {
		if why, bad := unavailable[c.SKU]; bad {
			t.Errorf("offered %q as a candidate (%s): %+v", c.SKU, why, c)
		}
	}
	if len(got) == 0 {
		t.Fatal("no candidates; the fixture has two available 80 GB types")
	}
}

// Availability is a property of a card AT a width, not of a card. Probing live,
// 35 of 48 types were obtainable as one GPU and 11 of 48 as eight, so a query
// that hardcoded the count would report capacity that is not there.
func TestCandidatesAskAtTheRequestedWidth(t *testing.T) {
	var queries []string
	p := candidateProvider(t, &queries)

	if _, err := p.Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 80, GpuCount: 8}); err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	if len(queries) != 1 {
		t.Fatalf("got %d queries, want 1", len(queries))
	}
	if !strings.Contains(queries[0], "gpuCount:8") {
		t.Errorf("query did not ask at the requested width:\n%s", queries[0])
	}
}

// An absent gpu_count means one, matching the rest of the provisioning path.
// Sending gpuCount:0 would ask RunPod a question about nothing.
func TestCandidatesDefaultToOneGPU(t *testing.T) {
	var queries []string
	p := candidateProvider(t, &queries)

	if _, err := p.Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 80}); err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	if !strings.Contains(queries[0], "gpuCount:1") {
		t.Errorf("query did not default to a single GPU:\n%s", queries[0])
	}
}

// RunPod's live catalog carries 48 types against our 16, and the extra ones are
// shapes we have never validated a deploy against. Offering something a create
// would then refuse is the failure this shares with the other two adapters.
func TestCandidatesFilterThroughTheSameResolverAsSpawn(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 80})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	for _, c := range got {
		if LookupSKU(c.SKU) == nil {
			t.Errorf("candidate %q is not in our catalog", c.SKU)
		}
		if c.VRAMGbPerGPU < 80 {
			t.Errorf("candidate %q has %d GB VRAM, below the stated floor", c.SKU, c.VRAMGbPerGPU)
		}
	}
}

// RunPod rents a GPU type and picks the datacenter itself, so there is no host
// to identify, no offer to name, and no region to record. Empty is the honest
// answer; a synthesized id would point at something that was never a place.
func TestCandidatesLeaveHostOfferAndRegionEmpty(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].HostID != "" || got[0].OfferID != "" || got[0].Region != "" {
		t.Errorf("HostID=%q OfferID=%q Region=%q, want all empty on a provider with none of those concepts",
			got[0].HostID, got[0].OfferID, got[0].Region)
	}
}

// The live price is half the reason to make the call. The fixture's 1.39
// disagrees with the static catalog's 1.79 on purpose.
func TestCandidatesUseTheLivePriceNotTheCatalog(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].PriceUSDPerHour != 1.39 {
		t.Errorf("price = %v, want the live 1.39", got[0].PriceUSDPerHour)
	}
	if catalog := LookupSKU("NVIDIA A100-SXM4-80GB"); catalog != nil &&
		got[0].PriceUSDPerHour == catalog.PriceUSDPerHour {
		t.Error("price came from the static catalog; the network call bought us nothing")
	}
}

// Read-only by contract. GraphQL puts reads and writes on one endpoint, so
// unlike the REST adapters there is no URL to assert on: the query text is the
// only thing separating a listing from a mutation.
func TestCandidatesNeverMutate(t *testing.T) {
	var queries []string
	p := candidateProvider(t, &queries)

	if _, err := p.Candidates(context.Background(), &provisionerv1.ResourceRequirements{}); err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	for _, q := range queries {
		if strings.Contains(q, "mutation") || strings.Contains(q, "updateUserSettings") {
			t.Errorf("Candidates sent a mutation:\n%s", q)
		}
		if !strings.HasPrefix(strings.TrimSpace(q), "query") {
			t.Errorf("Candidates sent something that is not a query:\n%s", q)
		}
	}
}

// The stock level is the strongest signal RunPod gives, and it stays in Attrs
// rather than becoming a typed field because no second provider reports
// anything comparable. Lambda's availability is binary and expressed by
// omission; Vast has no notion of stock at all.
func TestCandidatesCarryStockStatusAsAProviderAttr(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].Attrs["stock_status"] != "Medium" {
		t.Errorf("stock_status = %q, want Medium", got[0].Attrs["stock_status"])
	}
}

// A bid price that is not below the on-demand price is not a reclaimable tier,
// it is the same rental relabelled. Probing live, RunPod reported the two as
// equal on all 38 available shapes, so reporting them as reclaimable would
// claim a discount that does not exist and promise an interruptible rental
// this endpoint gives no way to verify.
func TestCandidatesRejectABidThatIsNotADiscount(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"gpuTypes":[
		  {"id":"NVIDIA A100-SXM4-80GB","memoryInGb":80,
		   "lowestPrice":{"uninterruptablePrice":1.39,"minimumBidPrice":1.39,"stockStatus":"Low"}},
		  {"id":"NVIDIA A100 80GB PCIe","memoryInGb":80,
		   "lowestPrice":{"uninterruptablePrice":1.19,"minimumBidPrice":0.40,"stockStatus":"Low"}}
		]}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	rt := &rewriteTransport{toHost: strings.TrimPrefix(srv.URL, "http://")}
	p := New(NewClient("k", WithHTTPClient(&http.Client{Transport: rt})))

	got, err := p.Candidates(context.Background(), &provisionerv1.ResourceRequirements{
		MinVramGb:     80,
		ReclaimPolicy: provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED,
	})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("got %d candidates, want only the one with a real discount: %+v", len(got), got)
	}
	if got[0].SKU != "NVIDIA A100 80GB PCIe" || got[0].PriceUSDPerHour != 0.40 {
		t.Errorf("got %s at $%.2f, want the PCIe shape at its 0.40 bid", got[0].SKU, got[0].PriceUSDPerHour)
	}
	if !got[0].Reclaimable {
		t.Error("the surviving candidate was not marked reclaimable")
	}
}
