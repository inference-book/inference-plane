package runpod

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

// gpuTypesQuery asks for every GPU type plus what it would cost and whether
// any can be had at the requested width.
//
// lowestPrice takes the gpuCount, and that argument is the whole reason this
// is worth a network call. Availability is not a property of a card, it is a
// property of a card AT a width: probing live on 2026-08-15, 35 of 48 types
// were obtainable as a single GPU and only 11 of 48 as eight. A catalog cannot
// express that and neither can --dry-run.
//
// GraphQL rather than REST because rest.runpod.io/v1 has no catalog endpoint
// at all (GET /v1/gpus 400s with "that path does not exist in the
// specification"). Same split as SSH keys, which is why gqlPost already
// exists.
const gpuTypesQuery = `query { gpuTypes { id displayName memoryInGb secureCloud communityCloud lowestPrice(input:{gpuCount:%d}) { uninterruptablePrice stockStatus } } }`

// gpuTypesData is the decode target for gpuTypesQuery.
type gpuTypesData struct {
	GPUTypes []struct {
		ID             string `json:"id"`
		DisplayName    string `json:"displayName"`
		MemoryInGb     int    `json:"memoryInGb"`
		SecureCloud    bool   `json:"secureCloud"`
		CommunityCloud bool   `json:"communityCloud"`
		LowestPrice    struct {
			// UninterruptablePrice is the on-demand rate for the WHOLE shape at
			// the requested gpuCount, not per card. Pointer because RunPod
			// sends null for a type nobody is currently offering, and a plain
			// float64 would turn "unavailable" into a free GPU.
			UninterruptablePrice *float64 `json:"uninterruptablePrice"`

			// StockStatus is "Low" / "Medium" / "High", or null when the type
			// cannot be had at this width. Null is the availability signal, so
			// it must stay distinguishable from a present-but-low reading.
			StockStatus *string `json:"stockStatus"`
		} `json:"lowestPrice"`
	} `json:"gpuTypes"`
}

// Candidates satisfies provisioners.CandidateSource.
//
// RunPod sits between the other two implementations, which is why it was worth
// probing rather than assuming. It is not a marketplace, so like Lambda there
// is no host to identify and no offer to name. But unlike Lambda it publishes
// no regions either: RunPod schedules wherever it has capacity, which is why
// --dry-run already prints "(unpinned)" for it. So a RunPod candidate is a GPU
// type and nothing more locational than that.
//
// What it does publish, and what makes this worth a call, is a live price and
// a stock level per width. Both are facts the static catalog cannot hold, and
// the stock level is the closest thing any of our three providers gives to a
// direct answer to "can I actually have this right now".
//
// The earlier note on CandidateSource guessed this would add nothing over the
// static table. That was wrong, and measurably so.
func (p *Provider) Candidates(ctx context.Context, reqs *provisionerv1.ResourceRequirements) ([]provisioners.Candidate, error) {
	if reqs == nil {
		reqs = &provisionerv1.ResourceRequirements{}
	}
	gpuCount := int(reqs.GetGpuCount())
	if gpuCount <= 0 {
		gpuCount = 1
	}

	raw, err := p.gqlPost(ctx, fmt.Sprintf(gpuTypesQuery, gpuCount))
	if err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "candidates", err, 0)
	}
	var data gpuTypesData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "candidates",
			fmt.Errorf("decode gpuTypes: %w", err), 0)
	}

	// The same resolver the rent path uses decides which types are eligible,
	// so a listing cannot offer something a create would then refuse. RunPod's
	// live catalog carries 48 types against our 16, and the extra 32 are ones
	// we have never validated a deploy against.
	var eligible map[string]bool
	if s := reqs.GetSku(); s != "" {
		eligible = map[string]bool{s: true}
	} else {
		eligible = map[string]bool{}
		for _, id := range MatchSKUs(reqs) {
			eligible[id] = true
		}
	}

	var out []provisioners.Candidate
	for _, g := range data.GPUTypes {
		if !eligible[g.ID] {
			continue
		}
		// A null stock status means this type cannot be had at this width at
		// all. Reporting it would put a row in front of an operator that they
		// cannot act on, which is the same call Lambda's empty-capacity case
		// makes.
		if g.LowestPrice.StockStatus == nil || g.LowestPrice.UninterruptablePrice == nil {
			continue
		}

		var (
			family fabric.Family
			vramGb = g.MemoryInGb
		)
		if spec := LookupSKU(g.ID); spec != nil {
			family = spec.Family
		}

		out = append(out, provisioners.Candidate{
			// No HostID, no OfferID, and no Region. RunPod rents a GPU type
			// and picks the datacenter itself, so there is nothing here to
			// name that would still mean anything an hour later.
			SKU:             g.ID,
			PriceUSDPerHour: *g.LowestPrice.UninterruptablePrice,
			GPUCount:        gpuCount,
			VRAMGbPerGPU:    vramGb,
			Fabric:          fabric.Resolve(fabric.Observation{Family: family}),
			Attrs: map[string]string{
				"stock_status":    *g.LowestPrice.StockStatus,
				"secure_cloud":    strconv.FormatBool(g.SecureCloud),
				"community_cloud": strconv.FormatBool(g.CommunityCloud),
			},
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].PriceUSDPerHour < out[j].PriceUSDPerHour
	})
	return out, nil
}
