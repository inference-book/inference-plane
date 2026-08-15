package lambdalabs

import (
	"context"
	"net/http"
	"sort"
	"strconv"

	skhttp "github.com/panyam/servicekit/http"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

// Candidates satisfies provisioners.CandidateSource.
//
// Lambda is the shape that tests whether the capability generalizes past a
// marketplace. Vast fans one SKU out into many offers from independent hosts,
// each with its own price and its own measured hardware. Lambda has fixed
// shapes at one published price, and the only thing that varies is which
// regions currently have any. So a Lambda candidate is one (shape, region)
// pair, it has no host identity and nothing to name as an offer, and those
// fields stay empty rather than being filled with something invented.
//
// What this adds over the static catalog is the two facts the catalog cannot
// hold: the live price, and whether the shape is actually available anywhere
// right now. Fifteen of Lambda's twenty-three shapes had capacity nowhere when
// this was written, and the catalog says nothing about that.
//
// /api/v1/instance-types is the endpoint. The adapter has carried a constant
// for it since it was written and never called it, and the comment on Spawn
// claiming Lambda "tells us which regions have capacity" described something
// that had never happened.
func (p *Provider) Candidates(ctx context.Context, reqs *provisionerv1.ResourceRequirements) ([]provisioners.Candidate, error) {
	if reqs == nil {
		reqs = &provisionerv1.ResourceRequirements{}
	}

	// Lambda sells on-demand only: /instance-types publishes one price per
	// shape and there is no bid, spot or preemptible tier anywhere in the API.
	// An operator who asked for reclaimable capacity asked for a discount, so
	// the honest answer is nothing rather than the full-price rental they did
	// not ask for (#288).
	if reqs.GetReclaimPolicy() == provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED {
		return nil, nil
	}

	req, err := p.client.newReq(http.MethodGet, pathInstanceTypes, nil, nil)
	if err != nil {
		return nil, wrapErr("candidates", err)
	}
	resp, err := skhttp.Call[instanceTypesResponse](ctx, req, p.client.callOpts()...)
	if err != nil {
		return nil, wrapErr("candidates", err)
	}

	// The same resolver the rent path uses decides which shapes are eligible,
	// so a listing cannot offer something a create would then refuse.
	var eligible map[string]bool
	if s := reqs.GetSku(); s != "" {
		eligible = map[string]bool{s: true}
	} else {
		eligible = map[string]bool{}
		for _, name := range MatchSKUs(reqs) {
			eligible[name] = true
		}
	}

	var out []provisioners.Candidate
	for name, entry := range resp.Data {
		if !eligible[name] {
			continue
		}
		// A shape with capacity nowhere contributes nothing. Reporting it with
		// an empty region would put a row in front of an operator that they
		// cannot act on, and the whole point of asking is to find what can
		// actually be had.
		for _, region := range entry.RegionsWithCapacity {
			out = append(out, typeToCandidate(name, &entry.InstanceType, region.Name))
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].PriceUSDPerHour < out[j].PriceUSDPerHour
	})
	return out, nil
}

// typeToCandidate projects one (shape, region) pair onto the shared form.
//
// Price and architecture come from the live response, which is the reason to
// make the call at all. VRAM comes from our catalog, because Lambda publishes
// system RAM and a GPU count but never the card's memory, and a candidate list
// that dropped VRAM would be missing the first thing anyone sizing a model
// looks at. An uncatalogued shape therefore reports 0 rather than a guess.
func typeToCandidate(name string, it *instanceTypeBlock, region string) provisioners.Candidate {
	var (
		family fabric.Family
		vramGb int
	)
	if spec := LookupSKU(name); spec != nil {
		family = spec.Family
		vramGb = spec.VRAMGb
	}

	return provisioners.Candidate{
		// No HostID and no OfferID on purpose. Lambda rents a shape in a
		// region, not a particular machine, so there is nothing stable to
		// record about a host and nothing short-lived to name as an offer.
		SKU:             name,
		Region:          region,
		PriceUSDPerHour: float64(it.PriceCentsPerHour) / 100,
		GPUCount:        it.Specs.GPUs,
		VRAMGbPerGPU:    vramGb,
		Architecture:    provisioners.NormalizeArch(it.Architecture),
		Fabric:          fabric.Resolve(fabric.Observation{Family: family}),
		Attrs: map[string]string{
			"vcpus":       strconv.Itoa(it.Specs.VCPUs),
			"memory_gib":  strconv.Itoa(it.Specs.MemoryGiB),
			"storage_gib": strconv.Itoa(it.Specs.StorageGiB),
		},
	}
}
