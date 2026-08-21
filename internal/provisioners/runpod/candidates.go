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
	"github.com/inference-book/inference-plane/internal/provisioners/skucatalog"
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
const gpuTypesQuery = `query { gpuTypes { id displayName memoryInGb secureCloud communityCloud lowestPrice(input:{gpuCount:%d}) { uninterruptablePrice minimumBidPrice stockStatus } } dataCenters { id location storageSupport gpuAvailability(input:{gpuCount:%d}) { gpuTypeId available stockStatus } } }`

// The dataCenters half is what says WHERE the capacity is, at the same
// width, and RunPod does publish it. This adapter used to say otherwise:
// "RunPod schedules wherever it has capacity" described the rent path's
// default rather than what the API knows.
//
// The datacenter is decision-relevant rather than trivia, because a network
// volume is locked to one. A model staged into the wrong datacenter is a
// warm cache no deploy can ever mount, and Spawn already accepts a region
// as a best-effort pin (dataCenterIds), so the answer is actionable.
//
// Found while planning #358: RunPod had eight-card capacity in exactly one
// datacenter, and that one supports no volumes at all. Nothing in `iplane
// capacity` could say so, which sent the planning to GraphQL by hand.

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

			// MinimumBidPrice is the interruptible rate for the same shape.
			// RunPod can reclaim a bid rental when someone outbids it, which
			// is why it is cheaper. Pointer for the same reason as above.
			MinimumBidPrice *float64 `json:"minimumBidPrice"`

			// StockStatus is "Low" / "Medium" / "High", or null when the type
			// cannot be had at this width. Null is the availability signal, so
			// it must stay distinguishable from a present-but-low reading.
			StockStatus *string `json:"stockStatus"`
		} `json:"lowestPrice"`
	} `json:"gpuTypes"`

	DataCenters []struct {
		ID       string `json:"id"`
		Location string `json:"location"`
		// StorageSupport is whether a network volume can live here, which
		// decides where `iplane model pin` can stage a model.
		StorageSupport bool `json:"storageSupport"`
		// GPUAvailability is per width, the same gpuCount the price query
		// asks about, so "available" means available AS the shape asked for.
		GPUAvailability []struct {
			GPUTypeID   string  `json:"gpuTypeId"`
			Available   bool    `json:"available"`
			StockStatus *string `json:"stockStatus"`
		} `json:"gpuAvailability"`
	} `json:"dataCenters"`
}

// datacenter is one place a GPU type can be had, reduced to what a
// candidate carries.
type datacenter struct {
	id      string
	storage bool
	stock   string
}

// datacentersByType inverts the datacenter list into a lookup from GPU type
// to the places currently holding it at the requested width.
//
// Unavailable entries are dropped rather than recorded as "here but empty",
// since a candidate an operator cannot act on is the thing this listing
// exists to avoid.
func datacentersByType(data gpuTypesData) map[string][]datacenter {
	out := map[string][]datacenter{}
	for _, dc := range data.DataCenters {
		for _, g := range dc.GPUAvailability {
			if !g.Available {
				continue
			}
			stock := ""
			if g.StockStatus != nil {
				stock = *g.StockStatus
			}
			out[g.GPUTypeID] = append(out[g.GPUTypeID], datacenter{id: dc.ID, storage: dc.StorageSupport, stock: stock})
		}
	}
	return out
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
func (p *Provider) Candidates(ctx context.Context, reqs *provisionerv1.ResourceRequirements) ([]*provisioners.Candidate, error) {
	if reqs == nil {
		reqs = &provisionerv1.ResourceRequirements{}
	}
	gpuCount := int(reqs.GetGpuCount())
	if gpuCount <= 0 {
		gpuCount = 1
	}

	raw, err := p.gqlPost(ctx, fmt.Sprintf(gpuTypesQuery, gpuCount, gpuCount))
	if err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "candidates", err, 0)
	}
	var data gpuTypesData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, provisioners.NewProviderError(p.Name(), "candidates",
			fmt.Errorf("decode gpuTypes: %w", err), 0)
	}

	inDatacenters := datacentersByType(data)

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

	var out []*provisioners.Candidate
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

		// The tier the operator asked for is the tier we price. A reclaimable
		// request that came back quoted at the on-demand rate would look like
		// the discount did not exist.
		price, reclaimable := *g.LowestPrice.UninterruptablePrice, false
		if reqs.GetReclaimPolicy() == provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED {
			// A bid price that is not below the on-demand price is not a
			// reclaimable tier, it is the same rental relabelled. Probing live
			// on 2026-08-15, minimumBidPrice equalled uninterruptablePrice on
			// all 38 available shapes, so in practice RunPod contributes
			// nothing to a reclaimable search today.
			//
			// Reporting those as reclaimable would claim a discount that does
			// not exist AND promise an interruptible rental this endpoint gives
			// us no way to verify. Dropping them is the same call the resolver
			// makes for an unknown fabric: we will not vouch for what the
			// provider did not tell us.
			if g.LowestPrice.MinimumBidPrice == nil ||
				*g.LowestPrice.MinimumBidPrice >= *g.LowestPrice.UninterruptablePrice {
				continue
			}
			price, reclaimable = *g.LowestPrice.MinimumBidPrice, true
		}

		// vramGb stays the live API figure, which is what an operator
		// filters on. exactBytes comes off the catalog row instead,
		// because the two want opposite error directions (#323).
		var (
			family     fabric.Family
			vramGb     = g.MemoryInGb
			exactBytes int64
		)
		if spec := LookupSKU(g.ID); spec != nil {
			family = spec.Family
			exactBytes = skucatalog.ExactVRAMBytes(spec.VRAMGb)
		}

		res := fabric.Resolve(fabric.Observation{Family: family})

		// One row per datacenter holding this type, because that is the
		// grain at which the answer is actionable: a volume lives in one,
		// and Spawn can pin to one. A type no datacenter list mentions
		// still gets a row with no region, since the datacenter view is
		// extra information and losing it must not lose the candidate.
		places := inDatacenters[g.ID]
		if len(places) == 0 {
			places = []datacenter{{}}
		}
		for _, dc := range places {
			stock := *g.LowestPrice.StockStatus
			if dc.stock != "" {
				// The datacenter's own reading, since "Low here, High
				// there" is exactly the distinction the type-level figure
				// flattens.
				stock = dc.stock
			}
			attrs := map[string]string{
				"stock_status":    stock,
				"secure_cloud":    strconv.FormatBool(g.SecureCloud),
				"community_cloud": strconv.FormatBool(g.CommunityCloud),
			}
			if dc.id != "" {
				// Whether a network volume can live here, which is what
				// decides where a model can be staged for a warm deploy.
				attrs["datacenter_storage"] = strconv.FormatBool(dc.storage)
			}
			out = append(out, &provisioners.Candidate{
				// Still no HostID and no OfferID: RunPod is not a
				// marketplace, so there is no host to name and nothing
				// short-lived to identify.
				Sku:              g.ID,
				Region:           dc.id,
				PriceUsdPerHour:  price,
				Reclaimable:      reclaimable,
				GpuCount:         int32(gpuCount),
				VramGbPerGpu:     int32(vramGb),
				VramBytesPerGpu:  exactBytes,
				FabricScope:      res.Scope,
				FabricSource:     res.Source,
				FabricGbps:       res.Gbps,
				FabricTechnology: res.Technology,
				Attrs:            attrs,
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].GetPriceUsdPerHour() < out[j].GetPriceUsdPerHour()
	})
	return out, nil
}
