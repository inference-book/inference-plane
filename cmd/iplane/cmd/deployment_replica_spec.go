package cmd

import (
	"fmt"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// parseReplicaSpecs converts repeated `--replica '<provider>:<class>'`
// flag values into proto ReplicaSpec messages. Empty input returns
// nil so the caller can branch on "heterogeneous form active" via a
// len() check.
//
// Form: `provider:class` (e.g., `runpod:small`, `vast:medium`).
// Region is not in this flag form -- operators who need a per-replica
// region drop down to the proto API or a future expanded flag form
// (--replica 'provider=runpod,region=us-east,class=small').
//
// Errors point the operator at the right form rather than silently
// dropping malformed entries.
func parseReplicaSpecs(raw []string) ([]*provisionerv1.ReplicaSpec, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]*provisionerv1.ReplicaSpec, 0, len(raw))
	for i, r := range raw {
		r = strings.TrimSpace(r)
		if r == "" {
			return nil, fmt.Errorf("--replica entry %d is empty", i)
		}
		parts := strings.SplitN(r, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, fmt.Errorf("--replica entry %d %q is malformed (want 'provider:class', e.g. 'runpod:small')", i, r)
		}
		provider := strings.TrimSpace(parts[0])
		class := strings.TrimSpace(parts[1])
		out = append(out, &provisionerv1.ReplicaSpec{
			Provider: provider,
			Requirements: &provisionerv1.ResourceRequirements{
				Class: class,
			},
		})
	}
	return out, nil
}
