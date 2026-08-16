package vast

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// The capability's central promise. A caller has to be able to run this while
// deciding, so anything that rents, reserves, or otherwise creates billable
// state breaks the contract that makes it useful at all.
func TestCandidatesNeverRents(t *testing.T) {
	var rentCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, bundlesResponse{Offers: []offerSummary{
			{ID: 111, MachineID: 6566, GpuName: "A100 SXM4", NumGPUs: 4, GpuRAM: 81920, DphTotal: 1.30},
		}})
	})
	// Any other path is a call this command must never make. /asks/ is the
	// rent endpoint specifically.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rentCalls++
		w.WriteHeader(http.StatusInternalServerError)
	})
	p, _ := newTestProvider(t, mux)

	if _, err := p.Candidates(context.Background(), &provisionerv1.ResourceRequirements{MinVramGb: 80}); err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if rentCalls != 0 {
		t.Errorf("Candidates made %d non-search call(s); it must only ever read the bundles endpoint", rentCalls)
	}
}

// The host and the offer are different lifetimes. An offer vanishes the moment
// somebody rents it; the machine behind it is still the same machine, and it
// is the machine you write down when a host turns out to be broken (#214).
// Collapsing the two would make that note useless within the hour.
func TestCandidatesSeparatesHostFromOffer(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, bundlesResponse{Offers: []offerSummary{
			{ID: 2210418, MachineID: 6566, GpuName: "A100 SXM4", NumGPUs: 4, GpuRAM: 81920, DphTotal: 1.30},
		}})
	})
	p, _ := newTestProvider(t, mux)

	got, err := p.Candidates(context.Background(), &provisionerv1.ResourceRequirements{Sku: "A100_SXM4"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].GetHostId() != "6566" {
		t.Errorf("HostID = %q, want the machine_id 6566", got[0].GetHostId())
	}
	if got[0].GetOfferId() != "2210418" {
		t.Errorf("OfferID = %q, want the offer id 2210418", got[0].GetOfferId())
	}
}

// A host that reports no bandwidth is not a host with zero bandwidth. Both
// arrive as a missing or zero bw_nvlink, and a candidate list that rendered
// them the same would let a host publishing nothing compare favourably against
// one that published a real low number.
func TestCandidatesKeepsFabricProvenance(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, bundlesResponse{Offers: []offerSummary{
			// Bridge-capable card with a real reading: measured.
			{ID: 1, MachineID: 10, GpuName: "A100 PCIE", NumGPUs: 2, GpuRAM: 81920, DphTotal: 1.10, BwNvlink: ptr(300)},
			// Same card, no reading at all: unknown, never "none".
			{ID: 2, MachineID: 11, GpuName: "A100 PCIE", NumGPUs: 2, GpuRAM: 81920, DphTotal: 1.20},
		}})
	})
	p, _ := newTestProvider(t, mux)

	got, err := p.Candidates(context.Background(), &provisionerv1.ResourceRequirements{Sku: "A100_PCIE"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d candidates, want 2", len(got))
	}
	if got[0].GetFabricSource() != provisionerv1.FabricSource_FABRIC_SOURCE_MEASURED {
		t.Errorf("measured host source = %v, want MEASURED", got[0].GetFabricSource())
	}
	if got[1].GetFabricSource() != provisionerv1.FabricSource_FABRIC_SOURCE_UNKNOWN {
		t.Errorf("unmeasured host source = %v, want UNKNOWN rather than a verdict", got[1].GetFabricSource())
	}
}

// Offers come back per SKU, and the operator's question spans SKUs. A list
// ordered per-SKU would put a dearer card first whenever the resolver happened
// to try it first.
func TestCandidatesOrderCheapestAcrossSKUs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		var q map[string]any
		_ = json.Unmarshal([]byte(r.URL.Query().Get("q")), &q)
		name, _ := q["gpu_name"].(map[string]any)["eq"].(string)
		// The dearer SKU answers first in catalog order; the cheaper one later.
		switch name {
		case "A100 PCIE":
			writeJSON(w, bundlesResponse{Offers: []offerSummary{
				{ID: 1, MachineID: 10, GpuName: name, NumGPUs: 1, GpuRAM: 81920, DphTotal: 2.40},
			}})
		default:
			writeJSON(w, bundlesResponse{Offers: []offerSummary{
				{ID: 2, MachineID: 11, GpuName: name, NumGPUs: 1, GpuRAM: 81920, DphTotal: 0.90},
			}})
		}
	})
	p, _ := newTestProvider(t, mux)

	got, err := p.Candidates(context.Background(), &provisionerv1.ResourceRequirements{MinVramGb: 80})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) < 2 {
		t.Fatalf("got %d candidates, want at least 2", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].GetPriceUsdPerHour() > got[i].GetPriceUsdPerHour() {
			t.Fatalf("candidates not cheapest-first: %v", prices(got))
		}
	}
}

// The listing and the rent path must ask the marketplace the same question. If
// they built their queries separately, a filter added to one would silently not
// apply to the other, and a candidate list that disagrees with what Spawn would
// pick is worse than none: it reads as a promise.
func TestCandidatesUseTheSameQueryAsSpawn(t *testing.T) {
	var queries []map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		var q map[string]any
		_ = json.Unmarshal([]byte(r.URL.Query().Get("q")), &q)
		queries = append(queries, q)
		writeJSON(w, bundlesResponse{Offers: nil})
	})
	p, _ := newTestProvider(t, mux)

	reqs := &provisionerv1.ResourceRequirements{
		Sku:         "A100_SXM4",
		GpuCount:    4,
		MinDiskGb:   150,
		FabricScope: provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	}
	if _, err := p.findOffer(context.Background(), "A100_SXM4", 4, 150, reqs); err != nil {
		t.Fatalf("findOffer: %v", err)
	}
	if _, err := p.Candidates(context.Background(), reqs); err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(queries) != 2 {
		t.Fatalf("got %d queries, want one from each path", len(queries))
	}

	// Everything except the page size has to match, because the page size is
	// the one thing the two paths legitimately want differently.
	for _, key := range []string{"gpu_name", "num_gpus", "rentable", "disk_space", "gpu_ram", "bw_nvlink"} {
		a, _ := json.Marshal(queries[0][key])
		b, _ := json.Marshal(queries[1][key])
		if string(a) != string(b) {
			t.Errorf("query key %q differs between spawn and candidates: %s vs %s", key, a, b)
		}
	}
}

func prices(cs []*provisioners.Candidate) []float64 {
	out := make([]float64, len(cs))
	for i, c := range cs {
		out[i] = c.GetPriceUsdPerHour()
	}
	return out
}

// Vast sells the same machine two ways, and the tier the operator asked for is
// the tier that gets priced. Quoting the bid floor for an on-demand rental
// would understate what they are about to pay; quoting on-demand for a
// reclaimable request would hide the discount they asked for.
func TestCandidatesPriceTheRequestedTier(t *testing.T) {
	offer := offerSummary{
		ID: 1, MachineID: 10, GpuName: "A100 SXM4", NumGPUs: 1, GpuRAM: 81920,
		DphTotal: 0.83, MinBid: 0.13,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, bundlesResponse{Offers: []offerSummary{offer}})
	})
	p, _ := newTestProvider(t, mux)

	onDemand, err := p.Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "A100_SXM4"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if onDemand[0].GetPriceUsdPerHour() != 0.83 || onDemand[0].GetReclaimable() {
		t.Errorf("default tier = $%.2f reclaimable=%v, want the on-demand 0.83",
			onDemand[0].GetPriceUsdPerHour(), onDemand[0].GetReclaimable())
	}

	reclaimable, err := p.Candidates(context.Background(), &provisionerv1.ResourceRequirements{
		Sku:           "A100_SXM4",
		ReclaimPolicy: provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED,
	})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if reclaimable[0].GetPriceUsdPerHour() != 0.13 || !reclaimable[0].GetReclaimable() {
		t.Errorf("reclaimable tier = $%.2f reclaimable=%v, want the bid floor 0.13",
			reclaimable[0].GetPriceUsdPerHour(), reclaimable[0].GetReclaimable())
	}
}
