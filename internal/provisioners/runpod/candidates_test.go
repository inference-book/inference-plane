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
],
"dataCenters":[
 {"id":"AP-IN-1","location":"India","storageSupport":false,
  "gpuAvailability":[{"gpuTypeId":"NVIDIA A100-SXM4-80GB","available":true,"stockStatus":"Low"}]},
 {"id":"EUR-IS-1","location":"Europe","storageSupport":true,
  "gpuAvailability":[{"gpuTypeId":"NVIDIA A100-SXM4-80GB","available":true,"stockStatus":"Medium"},
                     {"gpuTypeId":"NVIDIA L4","available":false,"stockStatus":null}]},
 {"id":"US-MO-1","location":"United States","storageSupport":false,
  "gpuAvailability":[{"gpuTypeId":"NVIDIA L4","available":true,"stockStatus":"High"}]}
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
		if why, bad := unavailable[c.GetSku()]; bad {
			t.Errorf("offered %q as a candidate (%s): %+v", c.GetSku(), why, c)
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
		if LookupSKU(c.GetSku()) == nil {
			t.Errorf("candidate %q is not in our catalog", c.GetSku())
		}
		if c.GetVramGbPerGpu() < 80 {
			t.Errorf("candidate %q has %d GB VRAM, below the stated floor", c.GetSku(), c.GetVramGbPerGpu())
		}
	}
}

// RunPod rents a GPU type and picks the datacenter itself, so there is no host
// to identify, no offer to name, and no region to record. Empty is the honest
// answer; a synthesized id would point at something that was never a place.
func TestCandidatesLeaveHostAndOfferEmpty(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	known := map[string]bool{"AP-IN-1": true, "EUR-IS-1": true, "US-MO-1": true}
	for _, c := range got {
		if c.GetHostId() != "" || c.GetOfferId() != "" {
			t.Errorf("HostID=%q OfferID=%q, want both empty on a provider with neither concept",
				c.GetHostId(), c.GetOfferId())
		}
		// Region is no longer empty, but it is still never invented: it is a
		// datacenter id the payload named, or nothing.
		if r := c.GetRegion(); r != "" && !known[r] {
			t.Errorf("region = %q, which no datacenter in the payload named", r)
		}
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
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	// Every row, since the price is a property of the type and one row per
	// datacenter must not let a stale figure in through a side door.
	for _, c := range got {
		if c.GetPriceUsdPerHour() != 1.39 {
			t.Errorf("%s price = %v, want the live 1.39", c.GetRegion(), c.GetPriceUsdPerHour())
		}
	}
	if catalog := LookupSKU("NVIDIA A100-SXM4-80GB"); catalog != nil &&
		got[0].GetPriceUsdPerHour() == catalog.PriceUSDPerHour {
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
	byRegion := map[string]string{}
	for _, c := range got {
		if s := c.GetAttrs()["stock_status"]; s == "" {
			t.Errorf("%s carries no stock_status", c.GetRegion())
		} else {
			byRegion[c.GetRegion()] = s
		}
	}
	// Per datacenter rather than one figure for the type. "Low here, Medium
	// there" is the distinction the type-level reading flattens, and it is
	// the one that decides where to send a deploy.
	if byRegion["AP-IN-1"] != "Low" || byRegion["EUR-IS-1"] != "Medium" {
		t.Errorf("stock by datacenter = %v, want AP-IN-1 Low and EUR-IS-1 Medium", byRegion)
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
	if got[0].GetSku() != "NVIDIA A100 80GB PCIe" || got[0].GetPriceUsdPerHour() != 0.40 {
		t.Errorf("got %s at $%.2f, want the PCIe shape at its 0.40 bid", got[0].GetSku(), got[0].GetPriceUsdPerHour())
	}
	if !got[0].GetReclaimable() {
		t.Error("the surviving candidate was not marked reclaimable")
	}
}

// The exact figure is the same literal all three adapters assert for an
// A100 80GB, so drift shows up here rather than as a silent disagreement
// between vendors about one physical card (#323).
func TestCandidatesCarryExactCardCapacity(t *testing.T) {
	p := candidateProvider(t, nil)

	got, err := p.Candidates(context.Background(), &provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	c := got[0]
	if c.GetVramGbPerGpu() != 80 {
		t.Errorf("advertised VRAM = %d, want the API's 80", c.GetVramGbPerGpu())
	}
	if want := int64(85_899_345_920); c.GetVramBytesPerGpu() != want {
		t.Errorf("exact VRAM = %d, want %d (80 GiB)", c.GetVramBytesPerGpu(), want)
	}
}

// RunPod does publish where its capacity is, and this adapter used to say
// it did not. `dataCenters { gpuAvailability(gpuCount) }` answers it per
// width, and the datacenter is decision-relevant rather than trivia: a
// network volume is locked to one, so a model staged in the wrong place is
// a warm cache no deploy can mount.
//
// Found the hard way while planning #358. RunPod had eight-card capacity in
// exactly one datacenter and that one supports no volumes, which `iplane
// capacity` had no way to say.
func TestCandidatesReportWhereTheCapacityIs(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB", GpuCount: 8})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	byRegion := map[string]*provisionerv1.Candidate{}
	for _, c := range got {
		byRegion[c.GetRegion()] = c
	}
	if len(byRegion) != 2 {
		t.Fatalf("want one candidate per datacenter holding the type, got %d: %+v", len(got), got)
	}
	for _, want := range []string{"AP-IN-1", "EUR-IS-1"} {
		if byRegion[want] == nil {
			t.Errorf("no candidate for %s: %+v", want, got)
		}
	}
	// The datacenter a volume can live in is the whole reason to report it.
	if s := byRegion["EUR-IS-1"].GetAttrs()["datacenter_storage"]; s != "true" {
		t.Errorf("EUR-IS-1 storage attr = %q, want true", s)
	}
	if s := byRegion["AP-IN-1"].GetAttrs()["datacenter_storage"]; s != "false" {
		t.Errorf("AP-IN-1 storage attr = %q, want false", s)
	}
	// Stock is per datacenter, not one figure for the type.
	if s := byRegion["EUR-IS-1"].GetAttrs()["stock_status"]; s != "Medium" {
		t.Errorf("EUR-IS-1 stock = %q, want the datacenter's own Medium", s)
	}
}

// A datacenter that lists the type as unavailable is not a place you can
// have it. Reporting it would put a row in front of an operator that they
// cannot act on, which is the same call the null-stock filter makes one
// level up.
func TestCandidatesSkipDatacentersThatHaveNone(t *testing.T) {
	got, err := candidateProvider(t, nil).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA L4"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	for _, c := range got {
		// The fixture has EUR-IS-1 holding an L4 entry with available:false.
		if c.GetRegion() == "EUR-IS-1" {
			t.Errorf("offered an L4 in a datacenter that reported none: %+v", c)
		}
	}
	if len(got) != 1 || got[0].GetRegion() != "US-MO-1" {
		t.Errorf("want the one datacenter that has it, got %+v", got)
	}
}

// The datacenter view is extra information, so losing it must not lose the
// candidate. A type the price query says is available, that no datacenter
// list mentions, is still available: RunPod schedules it somewhere.
func TestCandidatesSurviveADatacenterListThatSaysNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"gpuTypes":[
		 {"id":"NVIDIA A100-SXM4-80GB","displayName":"A100 SXM","memoryInGb":80,
		  "secureCloud":true,"communityCloud":true,
		  "lowestPrice":{"uninterruptablePrice":1.39,"stockStatus":"Medium"}}],
		 "dataCenters":[]}}`))
	}))
	t.Cleanup(srv.Close)
	rt := &rewriteTransport{toHost: strings.TrimPrefix(srv.URL, "http://")}
	p := New(NewClient("test-api-key", WithHTTPClient(&http.Client{Transport: rt})))

	got, err := p.Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "NVIDIA A100-SXM4-80GB", GpuCount: 8})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want the type reported once with no region, got %d: %+v", len(got), got)
	}
	if r := got[0].GetRegion(); r != "" {
		t.Errorf("region = %q, want empty when no datacenter named it", r)
	}
}
