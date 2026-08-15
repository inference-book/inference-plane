package provisioners

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/fabric"
)

// CandidateSource is an optional Provider capability for providers that can
// answer "what would you give me for these requirements, and is it any good"
// without renting anything. Asserted via provider.(CandidateSource), the same
// runtime opt-in as Deployer / KeyRegistrar / VolumeManager / FailureReporter.
//
// The gap it closes: Provider goes from requirements straight to Spawn, and
// Spawn spends money. Qualifying hosts for the Ch 10 fabric A/B was done by
// hand three times on 2026-08-11, with raw marketplace queries outside iplane
// to see the candidate set and then throwaway one-GPU rentals to find out
// whether a host could actually start a GPU container. Two of those probes
// found broken hosts, which is the outcome that justifies probing, and none of
// it was expressible through iplane, so none of it is reproducible by a reader
// following a walkthrough.
//
// # Read-only and free by contract
//
// An implementation must never rent, reserve, or otherwise create billable
// state. A caller has to be able to run this in a loop while deciding, which
// is the whole point of it existing.
//
// # Which providers can answer, measured 2026-08-15
//
//   - vast: yes, and it is the reason the capability exists. A SKU on a
//     marketplace is not a catalog entry, it is a set of live offers from
//     independent hosts that vary in price, bandwidth, reliability, disk and
//     fabric. findOffer already computes exactly this list and discards
//     everything except the winner.
//
//   - lambdalabs: expected yes, not yet implemented. /instance-types carries
//     live prices and which regions currently have capacity for a shape. The
//     adapter already calls that endpoint and reads only the hardware specs
//     off it.
//
//   - runpod: unknown, deliberately left unimplemented pending a probe rather
//     than guessed at. Its /gpus endpoint gives prices, and whether it exposes
//     per-SKU availability is a question to answer against the live API, the
//     way FailureReporter's RunPod entry was settled by inducing both failure
//     modes. A catalog-only answer would add nothing over what --dry-run
//     already prints from the static table.
//
//   - local, external: no, and structurally so. Neither selects among
//     candidates. local is the one machine the operator already has, and
//     external attaches to an engine somebody else is running.
type CandidateSource interface {
	Candidates(ctx context.Context, reqs *provisionerv1.ResourceRequirements) ([]Candidate, error)
}

// Candidate is one thing a provider would rent us, carrying what a placement
// decision actually turns on rather than the provider's whole record.
//
// Everything here is provider-reported. A field this type cannot fill stays
// zero, and a caller must read that as "not reported" rather than as a
// measurement, which is the same rule Fabric carries explicitly below.
type Candidate struct {
	// HostID identifies the physical machine behind the offer, stable across
	// the offers that come and go on it. Separate from OfferID because on a
	// marketplace they are different lifetimes: an offer disappears when
	// somebody rents it, while the host is still the host, and "do not place
	// on this box again" (#214) is a statement about the host.
	//
	// Empty when the provider has no notion of a host distinct from the thing
	// being rented.
	HostID string

	// OfferID is what a Spawn would actually name. On a marketplace this is
	// short-lived, so it is useful for acting on a candidate immediately and
	// worthless for remembering one.
	OfferID string

	// SKU is the provider's own token for the card, in the adapter's
	// vocabulary rather than the marketplace's wire form.
	SKU string

	// Region is the provider's placement label, empty where a provider does
	// not pin or does not say.
	Region string

	// PriceUSDPerHour is what the provider quotes for this candidate now. This
	// is a live figure rather than the static catalog estimate, which is most
	// of why asking is worth a network call.
	PriceUSDPerHour float64

	// GPUCount and VRAMGbPerGPU describe the shape on offer. Both come from
	// the provider's own record, so they can disagree with the catalog, and
	// where they do the provider is right.
	GPUCount     int
	VRAMGbPerGPU int

	// Fabric is the resolved interconnect verdict, carried whole rather than
	// as a bandwidth number so the source survives. A candidate that got here
	// on a bridge-capable card reads UNKNOWN, and a ranking that treated that
	// as a zero would let a vendor who publishes nothing beat one who does.
	Fabric fabric.Result

	// Attrs carries the provider-reported values the adapter filtered on, so
	// an operator can see why a candidate is in the list and check a floor
	// that was supposed to exclude something. Keys are the provider's own
	// field names. Diagnostic only, nothing branches on it.
	Attrs map[string]string
}

// ListCandidates asks one provider what it would offer for these
// requirements, without renting anything.
//
// Unimplemented rather than an empty list when the provider has no such
// capability, because "this provider cannot answer" and "this provider has no
// capacity right now" are different answers and collapsing them would tell an
// operator their requirements were unsatisfiable when nobody looked.
func (s *Service) ListCandidates(ctx context.Context, providerName string, reqs *provisionerv1.ResourceRequirements) ([]Candidate, error) {
	provider, ok := s.providers[providerName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "provider %q is not configured", providerName)
	}
	cs, ok := provider.(CandidateSource)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented,
			"provider %q cannot list candidates without renting one", providerName)
	}
	return cs.Candidates(ctx, reqs)
}
