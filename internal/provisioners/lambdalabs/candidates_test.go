package lambdalabs

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// instanceTypesJSON is the live payload shape, verified against the real API
// on 2026-08-15. Keyed by shape name rather than a list, which is the detail
// most likely to be got wrong from memory.
const instanceTypesJSON = `{"data":{
  "gpu_1x_a100_sxm4": {
    "instance_type": {"name":"gpu_1x_a100_sxm4","price_cents_per_hour":199,
      "architecture":"x86_64","specs":{"vcpus":30,"memory_gib":220,"storage_gib":512,"gpus":1}},
    "regions_with_capacity_available":[{"name":"us-east-1"},{"name":"us-west-2"}]},
  "gpu_1x_gh200": {
    "instance_type": {"name":"gpu_1x_gh200","price_cents_per_hour":229,
      "architecture":"arm64","specs":{"vcpus":64,"memory_gib":432,"storage_gib":4096,"gpus":1}},
    "regions_with_capacity_available":[{"name":"us-east-3"}]},
  "gpu_1x_h100_sxm5": {
    "instance_type": {"name":"gpu_1x_h100_sxm5","price_cents_per_hour":429,
      "architecture":"x86_64","specs":{"vcpus":26,"memory_gib":225,"storage_gib":1024,"gpus":1}},
    "regions_with_capacity_available":[]}
}}`

func candidateProvider(t *testing.T) *Provider {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instance-types", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(instanceTypesJSON))
	})
	p, _ := newTestProvider(t, mux)
	return p
}

// A shape with capacity nowhere is not a candidate. Reporting it with an empty
// region would put a row in front of an operator that they cannot act on, and
// finding what can actually be had is the entire question.
func TestCandidatesSkipShapesWithNoCapacity(t *testing.T) {
	got, err := candidateProvider(t).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 80})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	for _, c := range got {
		if c.GetSku() == "gpu_1x_h100_sxm5" {
			t.Errorf("a shape with an empty capacity list was offered as a candidate: %+v", c)
		}
	}
	if len(got) == 0 {
		t.Fatal("no candidates at all; the fixture has two shapes with capacity")
	}
}

// One candidate per region, because a shape available in two regions is two
// different things you can actually have, and an operator picking on latency or
// on where their weights are staged needs to see both.
func TestCandidatesFanOutPerRegion(t *testing.T) {
	got, err := candidateProvider(t).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "gpu_1x_a100_sxm4"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("got %d candidates, want one per region with capacity", len(got))
	}
	seen := map[string]bool{got[0].GetRegion(): true, got[1].Region: true}
	if !seen["us-east-1"] || !seen["us-west-2"] {
		t.Errorf("regions = %v, want us-east-1 and us-west-2", seen)
	}
}

// Lambda rents a shape in a region, not a particular machine. Inventing a host
// id would make "do not place here again" (#214) point at something that was
// never a place.
func TestCandidatesLeaveHostAndOfferEmpty(t *testing.T) {
	got, err := candidateProvider(t).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "gpu_1x_gh200"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].GetHostId() != "" || got[0].GetOfferId() != "" {
		t.Errorf("HostID=%q OfferID=%q, want both empty on a provider with no such concept",
			got[0].GetHostId(), got[0].GetOfferId())
	}
}

// The reason to make the call at all. A stale catalog price would make the
// listing agree with --dry-run and be worth nothing.
func TestCandidatesUseTheLivePriceNotTheCatalog(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instance-types", func(w http.ResponseWriter, r *http.Request) {
		// Deliberately disagrees with the static catalog's 1.99.
		_, _ = w.Write([]byte(`{"data":{"gpu_1x_a100_sxm4":{"instance_type":{
		  "name":"gpu_1x_a100_sxm4","price_cents_per_hour":777,"architecture":"x86_64",
		  "specs":{"gpus":1}},"regions_with_capacity_available":[{"name":"us-east-1"}]}}}`))
	})
	p, _ := newTestProvider(t, mux)

	got, err := p.Candidates(context.Background(), &provisionerv1.ResourceRequirements{Sku: "gpu_1x_a100_sxm4"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].GetPriceUsdPerHour() != 7.77 {
		t.Errorf("price = %v, want the live 7.77 rather than the catalog's figure", got[0].GetPriceUsdPerHour())
	}
	if catalog := LookupSKU("gpu_1x_a100_sxm4"); catalog != nil && got[0].GetPriceUsdPerHour() == catalog.PriceUSDPerHour {
		t.Error("price came from the static catalog; the network call bought us nothing")
	}
}

// An arm64 host needs an arm64 engine image, and Lambda's GH200 is the shape
// in this catalog where that bites. Providers spell the fact differently, so
// the adapter must normalize rather than pass the vendor's string through.
func TestCandidatesNormalizeArchitecture(t *testing.T) {
	got, err := candidateProvider(t).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 80})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	byName := map[string]*provisioners.Candidate{}
	for _, c := range got {
		byName[c.GetSku()] = c
	}
	if a := byName["gpu_1x_gh200"].GetArchitecture(); a != provisioners.ArchARM64 {
		t.Errorf("gh200 architecture = %q, want %q", a, provisioners.ArchARM64)
	}
	// "x86_64" on the wire, "amd64" once normalized. Passing the raw string
	// through would leave a caller comparing it against Vast's "amd64".
	if a := byName["gpu_1x_a100_sxm4"].GetArchitecture(); a != provisioners.ArchAMD64 {
		t.Errorf("a100 architecture = %q, want %q normalized from x86_64", a, provisioners.ArchAMD64)
	}
}

// Lambda publishes system RAM and a GPU count but never the card's memory, so
// VRAM comes from our catalog. An uncatalogued shape must report 0 rather than
// a guess, since a wrong VRAM figure is how a model gets sized onto a card it
// does not fit.
func TestCandidatesReportZeroVRAMForAnUncataloguedShape(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instance-types", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"gpu_1x_notinourcatalog":{"instance_type":{
		  "name":"gpu_1x_notinourcatalog","price_cents_per_hour":100,"architecture":"x86_64",
		  "specs":{"gpus":1}},"regions_with_capacity_available":[{"name":"us-east-1"}]}}}`))
	})
	p, _ := newTestProvider(t, mux)

	got, err := p.Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{Sku: "gpu_1x_notinourcatalog"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d candidates, want 1", len(got))
	}
	if got[0].GetVramGbPerGpu() != 0 {
		t.Errorf("VRAM = %d for a shape we have no catalog entry for, want 0 rather than a guess", got[0].GetVramGbPerGpu())
	}
}

// The listing must not offer something a create would refuse, so both go
// through the same resolver.
func TestCandidatesFilterThroughTheSameResolverAsSpawn(t *testing.T) {
	got, err := candidateProvider(t).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{MinVramGb: 96})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}

	for _, c := range got {
		spec := LookupSKU(c.GetSku())
		if spec == nil {
			t.Errorf("candidate %q is not in the catalog", c.GetSku())
			continue
		}
		if spec.VRAMGb < 96 {
			t.Errorf("candidate %q has %d GB VRAM, below the stated floor", c.GetSku(), spec.VRAMGb)
		}
	}
}

// Guards the decode target itself. The payload is a map keyed by shape name,
// not a list, and a wrong container type deserializes to an empty result that
// reads exactly like "no capacity anywhere".
func TestInstanceTypesDecodeIsKeyedByName(t *testing.T) {
	var resp instanceTypesResponse
	if err := json.Unmarshal([]byte(instanceTypesJSON), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 3 {
		t.Fatalf("decoded %d shapes, want 3", len(resp.Data))
	}
	if got := resp.Data["gpu_1x_gh200"].InstanceType.Architecture; got != "arm64" {
		t.Errorf("architecture = %q, want arm64", got)
	}
}

// Lambda has no bid, spot or preemptible tier anywhere in its API. An operator
// who asked for reclaimable capacity asked for a discount, so the honest
// answer is nothing rather than the full-price rental they did not ask for.
func TestCandidatesRefuseToSubstituteOnDemandForReclaimable(t *testing.T) {
	got, err := candidateProvider(t).Candidates(context.Background(),
		&provisionerv1.ResourceRequirements{
			MinVramGb:     80,
			ReclaimPolicy: provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED,
		})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d candidate(s) for a reclaimable request; Lambda has no such tier and must not quote on-demand instead: %+v", len(got), got)
	}
}

// Third of the cross-adapter agreement on one physical card (#323).
func TestCandidatesCarryExactCardCapacity(t *testing.T) {
	p := candidateProvider(t)

	got, err := p.Candidates(context.Background(), &provisionerv1.ResourceRequirements{Sku: "gpu_1x_a100_sxm4"})
	if err != nil {
		t.Fatalf("Candidates: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no candidates")
	}
	c := got[0]
	if c.GetVramGbPerGpu() != 80 {
		t.Errorf("advertised VRAM = %d, want the catalog's 80", c.GetVramGbPerGpu())
	}
	if want := int64(85_899_345_920); c.GetVramBytesPerGpu() != want {
		t.Errorf("exact VRAM = %d, want %d (80 GiB)", c.GetVramBytesPerGpu(), want)
	}
}
