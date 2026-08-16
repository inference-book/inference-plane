package provisioners

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
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
	Candidates(ctx context.Context, reqs *provisionerv1.ResourceRequirements) ([]*Candidate, error)
}

// Candidate is the generated provisionerv1.Candidate.
//
// Aliased rather than redeclared. The package doc's rule is that wire
// types travel as the generated messages with no parallel Go struct to keep
// in sync, and the first cut of this broke that rule: the Go-only shape is
// what left the capacity search with no RPC and therefore answering from the
// wrong host entirely (#304).
type Candidate = provisionerv1.Candidate

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

// CandidatesFrom asks one provider what it would offer for these
// requirements, without renting anything.
//
// The single-provider primitive. ListCandidates is the RPC over the
// fan-out; this is what one answer costs.
//
// Unimplemented rather than an empty list when the provider has no such
// capability, because "this provider cannot answer" and "this provider has no
// capacity right now" are different answers and collapsing them would tell an
// operator their requirements were unsatisfiable when nobody looked.
func (s *Service) CandidatesFrom(ctx context.Context, providerName string, reqs *provisionerv1.ResourceRequirements) ([]*Candidate, error) {
	provider, ok := s.providers[providerName]
	if !ok {
		return nil, status.Errorf(codes.NotFound, "provider %q is not configured", providerName)
	}
	cs, ok := provider.(CandidateSource)
	if !ok {
		return nil, status.Errorf(codes.Unimplemented,
			"provider %q cannot list candidates without renting one", providerName)
	}
	out, err := cs.Candidates(ctx, reqs)
	if err != nil {
		return nil, err
	}
	for _, c := range out {
		c.Provider = providerName
	}
	return out, nil
}
