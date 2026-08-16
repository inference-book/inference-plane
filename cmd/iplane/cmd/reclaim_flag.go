package cmd

import (
	"fmt"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// reclaimFlagUsage documents --reclaim in the operator's terms rather than in
// any one vendor's. Vast bids, RunPod bids, Lambda has no such tier at all.
const reclaimFlagUsage = `accept capacity the provider can take back: yes (cheaper, interruptible) | no (on-demand only). ` +
	`default is the provider's normal tier`

// parseReclaimPolicy maps the flag onto the proto enum.
//
// Unrecognized values are rejected rather than defaulting. A typo'd --reclaim
// silently meaning "no preference" would hand an operator the full-price tier
// they were explicitly trying to avoid, which is the failure #288 named.
func parseReclaimPolicy(s string) (provisionerv1.ReclaimPolicy, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return provisionerv1.ReclaimPolicy_RECLAIM_POLICY_UNSPECIFIED, nil
	case "yes", "true", "reclaimable":
		return provisionerv1.ReclaimPolicy_RECLAIM_POLICY_PREFERRED, nil
	case "no", "false", "on-demand":
		return provisionerv1.ReclaimPolicy_RECLAIM_POLICY_NEVER, nil
	default:
		return 0, fmt.Errorf("--reclaim %q: want yes or no", s)
	}
}
