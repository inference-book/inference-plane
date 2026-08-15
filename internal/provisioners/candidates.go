package provisioners

import (
	"context"
	"strings"

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
//   - lambdalabs: yes, and it is the shape that proved the capability is not
//     marketplace-specific. Fixed shapes at a published price, where the only
//     thing that varies is which regions have any right now. /instance-types
//     answers both, and probing live on 2026-08-15 fifteen of twenty-three
//     shapes had capacity in no region at all, which is precisely the fact a
//     static catalog cannot hold. A Lambda candidate has no host identity and
//     no offer id, and those fields stay empty rather than invented.
//
//   - runpod: yes, and the earlier guess here was wrong. This block used to
//     say a RunPod answer would add nothing over the static catalog. Probing
//     the live API on 2026-08-15 found otherwise: gpuTypes.lowestPrice takes a
//     gpuCount and returns both a live price and a stock level, and 35 of 48
//     types were obtainable as a single GPU against 11 of 48 as eight.
//     Availability is a property of a card AT a width, which is exactly what a
//     catalog cannot express. Note the REST API has no catalog endpoint at
//     all, so this is the one GraphQL read in the adapter besides SSH keys.
//     Neither a host nor a region: RunPod schedules where it likes.
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
	// the provider's own record where the provider reports them, so they can
	// disagree with the catalog, and where they do the provider is right.
	// VRAMGbPerGPU falls back to the catalog on providers that publish a GPU
	// count and a system-RAM figure but never the card's memory.
	GPUCount     int
	VRAMGbPerGPU int

	// Architecture is the host CPU architecture, normalized to one vocabulary
	// ("amd64", "arm64"), empty where the provider does not report it.
	//
	// It is a typed field rather than an Attrs entry because it decides
	// whether a deploy works at all: an arm64 host needs an engine image built
	// for arm64, and Lambda's GH200 shapes are arm64 while everything else in
	// our catalog is not. Both providers that report it spell it differently
	// (Vast says "amd64", Lambda says "x86_64" for the same thing), which is
	// exactly the normalization a shared shape exists to do.
	Architecture string

	// Fabric is the resolved interconnect verdict, carried whole rather than
	// as a bandwidth number so the source survives. A candidate that got here
	// on a bridge-capable card reads UNKNOWN, and a ranking that treated that
	// as a zero would let a vendor who publishes nothing beat one who does.
	Fabric fabric.Result

	// Attrs carries the provider-reported values the adapter filtered on, so
	// an operator can see why a candidate is in the list and check a floor
	// that was supposed to exclude something. Keys are the provider's own
	// field names. Diagnostic only, nothing branches on it.
	//
	// Deliberately NOT where cross-provider facts live. Anything here is an
	// untyped string under a provider's own key, so it cannot be compared
	// against another provider's record and must never feed a ranking. A fact
	// that turns out to matter on a second provider gets promoted to a typed
	// field and normalized, which is how Architecture arrived.
	Attrs map[string]string
}

// Architecture values, normalized across providers. Adapters map their own
// spelling onto these rather than passing the provider's string through, for
// the same reason fabric.Family exists: "amd64" and "x86_64" are one fact with
// two names, and a caller comparing candidates should never have to know which
// vendor said which.
const (
	ArchAMD64 = "amd64"
	ArchARM64 = "arm64"
)

// NormalizeArch maps a provider's architecture string onto the shared
// vocabulary, returning "" for anything unrecognized.
//
// Empty means "not reported" and must not be read as "probably x86". A wrong
// guess here produces a deploy that pulls an image the host cannot run, and
// the failure surfaces as a container that will not start rather than as
// anything naming the architecture.
func NormalizeArch(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "amd64", "x86_64", "x86-64":
		return ArchAMD64
	case "arm64", "aarch64":
		return ArchARM64
	default:
		return ""
	}
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
