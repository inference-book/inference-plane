package provisioners

import (
	"slices"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// MatchesTags reports whether a listed instance's recovered tags satisfy a
// List filter. Match-all: every key in the filter must be present on the
// instance with the same value.
//
// A filter key the instance does not carry excludes it. That is the whole
// point, and it is the opposite of what two adapters used to do. Vast and
// Lambda each read one prefix key of their own and dropped everything else,
// so `List({iplane-id: x})` returned the entire account rather than one
// instance. The Service then read the length of that list as an answer
// about x.
//
// The failure direction matters more than the rule. An ignored filter
// returns everything, which reads as "all of these are yours" and cannot be
// distinguished from a real answer. A strict filter returns nothing, which
// reads as "no match" and is at least the shape of a real answer to the
// question that was asked. Neither is a substitute for the caller asking a
// question the provider can answer, which is why the Service no longer
// filters on a tag an adapter cannot recover.
//
// An empty or nil filter matches everything, which is how a caller asks for
// the whole account on purpose.
func MatchesTags(tags, filter map[string]string) bool {
	for k, want := range filter {
		if tags[k] != want {
			return false
		}
	}
	return true
}

// FilterRefs returns the refs whose tags satisfy the filter. Adapters call
// it after whatever narrowing their own API supports, so the match-all rule
// lives in one place rather than in three hand-rolled loops.
//
// Never returns nil on a non-nil input, because a nil slice and an empty one
// mean the same thing to every caller here and an allocated empty slice
// keeps that true at the boundary.
func FilterRefs(refs []*provisionerv1.InstanceRef, filter map[string]string) []*provisionerv1.InstanceRef {
	if len(filter) == 0 {
		return refs
	}
	out := make([]*provisionerv1.InstanceRef, 0, len(refs))
	for _, ref := range refs {
		if MatchesTags(ref.GetTags(), filter) {
			out = append(out, ref)
		}
	}
	return out
}

// TagsOnly returns the filter with an adapter's own non-tag keys removed, so
// the remainder can be matched against recovered tags.
//
// Two adapters accept a private key alongside the tag vocabulary ("label-prefix"
// on Vast, "name-prefix" on Lambda) for the case that wants every instance
// iplane created rather than one named instance. Those keys are answered by
// the adapter's own narrowing and would match nothing if they reached the tag
// comparison.
//
// Returns nil when nothing is left, which FilterRefs reads as "no filter".
func TagsOnly(filter map[string]string, adapterKeys ...string) map[string]string {
	if len(filter) == 0 {
		return nil
	}
	out := make(map[string]string, len(filter))
	for k, v := range filter {
		if slices.Contains(adapterKeys, k) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
