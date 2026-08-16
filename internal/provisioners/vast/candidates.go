package vast

import (
	"context"
	"sort"
	"strconv"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

// candidateOfferLimit is how many offers per SKU the read-only listing asks
// for. Higher than the rent path's limit because the two are answering
// different questions: Spawn wants the cheapest thing it can have, and an
// operator comparing hosts wants to see the spread, including the offers just
// above the cheapest that might be worth the difference.
const candidateOfferLimit = 20

// Candidates satisfies provisioners.CandidateSource.
//
// Vast is the provider this capability was designed around. A gpu_name here is
// not a catalog entry but a set of live offers from independent hosts, varying
// in price, bandwidth, reliability, disk and fabric, and two of them in a 2026-08
// probe were quietly broken. Deciding among those without renting one is the
// whole job.
//
// The SKU list comes from the same resolver Spawn uses, and each SKU is
// searched with the same query builder, so the listing cannot drift from what
// Spawn would actually do. An operator-supplied sku short-circuits the catalog
// exactly as it does on the rent path.
//
// Offers are returned cheapest first across the whole set rather than grouped
// by SKU, because the operator's question is "what should I rent" and not
// "what does each SKU cost". A SKU whose offers all fail the marketplace
// quality floors contributes nothing and is not reported as an empty group.
func (p *Provider) Candidates(ctx context.Context, reqs *provisionerv1.ResourceRequirements) ([]provisioners.Candidate, error) {
	if reqs == nil {
		reqs = &provisionerv1.ResourceRequirements{}
	}

	var skus []string
	if s := reqs.GetSku(); s != "" {
		skus = []string{normalizeGpuName(s)}
	} else {
		skus = MatchSKUs(reqs)
	}

	gpuCount := int(reqs.GetGpuCount())
	if gpuCount <= 0 {
		gpuCount = 1
	}
	diskGB := int(reqs.GetMinDiskGb())
	if diskGB <= 0 {
		diskGB = defaultDiskGB
	}

	var out []provisioners.Candidate
	for _, sku := range skus {
		offers, err := p.searchOffers(ctx, sku, gpuCount, diskGB, reqs, candidateOfferLimit)
		if err != nil {
			return nil, provisioners.NewProviderError(p.Name(), "candidates", err, 0)
		}
		for i := range offers {
			out = append(out, offerToCandidate(sku, &offers[i], reqs.GetReclaimPolicy()))
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].PriceUSDPerHour < out[j].PriceUSDPerHour
	})
	return out, nil
}

// offerToCandidate projects one marketplace offer onto the provider-neutral
// shape, resolving the fabric through the same rules the rent path applies.
//
// sku is our catalog token rather than the offer's gpu_name, because those are
// not the same string for variant SKUs and the token is what an operator would
// pass back to --gpu-sku.
func offerToCandidate(sku string, o *offerSummary, reclaim provisionerv1.ReclaimPolicy) provisioners.Candidate {
	obs := fabric.Observation{}
	if spec := LookupSKU(sku); spec != nil {
		obs.Family = spec.Family
	}
	if o.BwNvlink != nil {
		obs.HasMeasurement = true
		obs.MeasuredGbps = fabric.GbpsFromGBps(*o.BwNvlink)
	}

	// Vast sells the same machine two ways. An interruptible rental is priced
	// at the bid floor and can be taken by a higher bidder; an on-demand one
	// is not. Quoting the cheaper number for a rental we would not actually
	// make would misprice the comparison, so the tier the operator asked for
	// is the tier we price.
	price, reclaimable := o.DphTotal, false
	if reclaim == provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED && o.MinBid > 0 {
		price, reclaimable = o.MinBid, true
	}

	return provisioners.Candidate{
		HostID:          strconv.Itoa(o.MachineID),
		OfferID:         strconv.Itoa(o.ID),
		SKU:             sku,
		Region:          o.GeoLocation,
		PriceUSDPerHour: price,
		Reclaimable:     reclaimable,
		GPUCount:        o.NumGPUs,
		VRAMGbPerGPU:    o.GpuRAM / 1000,
		Architecture:    provisioners.NormalizeArch(o.CPUArch),
		Fabric:          fabric.Resolve(obs),
		Attrs: map[string]string{
			"inet_down":    strconv.FormatFloat(o.InetDown, 'f', 0, 64),
			"reliability2": strconv.FormatFloat(o.Reliability2, 'f', 4, 64),
			"disk_space":   strconv.FormatFloat(o.DiskGB, 'f', 0, 64),
			"cpu_ram_gb":   strconv.Itoa(o.CPURam / 1000),
		},
	}
}
