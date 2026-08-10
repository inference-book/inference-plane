package cmd

import (
	"fmt"
	"sort"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// fabricScopes is the complete set of accepted --fabric values. One
// vocabulary, deliberately: the property, never the vendor's product name.
//
// An earlier version also accepted "nvlink", "rdma" and friends as aliases,
// on the theory that operators say those words and a CLI should meet them
// there. Dropped, for a reason worth recording. The alias promises a
// specificity we are structurally unable to honour: --fabric nvlink on a
// mixed fleet would return an AMD xGMI host, because the token can only
// widen to "any intra-node fabric". Narrowing it properly would mean
// branching on Hardware.fabric_technology, which the proto forbids as the
// thing that turns that string back into a vendor enum. Accepting a word
// whose obvious meaning we cannot implement is worse than refusing it.
//
// vendorHints below keeps the words useful without accepting them as input.
var fabricScopes = map[string]provisionerv1.FabricScope{
	"none":       provisionerv1.FabricScope_FABRIC_SCOPE_NONE,
	"intra-node": provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	"inter-node": provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE,
}

// vendorHints maps the vendor technology names an operator is likely to
// reach for onto the scope they belong to. Used ONLY to turn a rejection
// into a redirect, never to resolve a value: typing a brand name gets you a
// one-line correction naming the right token, not a silent reinterpretation.
var vendorHints = map[string]string{
	"nvlink":     "intra-node",
	"nvswitch":   "intra-node",
	"xgmi":       "intra-node",
	"rdma":       "inter-node",
	"infiniband": "inter-node",
	"roce":       "inter-node",
	"efa":        "inter-node",
}

// parseFabricScope resolves a --fabric value. An empty string means the
// operator expressed no preference and yields UNSPECIFIED, which every
// candidate satisfies (the Ch 6-9 default, and it must stay free).
func parseFabricScope(v string) (provisionerv1.FabricScope, error) {
	token := strings.ToLower(strings.TrimSpace(v))
	if token == "" {
		return provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED, nil
	}
	if scope, ok := fabricScopes[token]; ok {
		return scope, nil
	}
	unspec := provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED
	if scope, ok := vendorHints[token]; ok {
		return unspec, fmt.Errorf(
			"--fabric %q: %s is a vendor technology, not a fabric scope; use --fabric %s "+
				"(iplane selects on where a fabric reaches, so one scope covers every vendor's)",
			v, token, scope)
	}
	known := make([]string, 0, len(fabricScopes))
	for k := range fabricScopes {
		known = append(known, k)
	}
	sort.Strings(known)
	return unspec, fmt.Errorf("unknown --fabric %q (known: %s)", v, strings.Join(known, ", "))
}

// fabricFlagUsage is the shared help text. Kept in one place so the two
// commands that take the flag cannot drift apart.
const fabricFlagUsage = `required GPU interconnect: none | intra-node | inter-node. ` +
	`Intra-node is what NVLink, NVSwitch and AMD xGMI provide; inter-node is InfiniBand, RoCE or EFA. ` +
	`Needs --gpu-count >= 2. Candidates whose fabric the provider does not report are rejected, ` +
	`not assumed; use --sku to override`

const minFabricGbpsUsage = `minimum fabric bandwidth in gigabits/sec (not gigabytes; ` +
	`an A100's 600 GB/s NVLink is 4800). Requires --fabric`

// fabricScopeLabel renders a scope in the CLI's canonical vocabulary. Not the
// proto enum name, which leaks FABRIC_SCOPE_ prefixes into operator output.
func fabricScopeLabel(scope provisionerv1.FabricScope) string {
	switch scope {
	case provisionerv1.FabricScope_FABRIC_SCOPE_NONE:
		return "none"
	case provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE:
		return "intra-node"
	case provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE:
		return "inter-node"
	default:
		return "unspecified"
	}
}

// fabricBandwidthNote renders an optional bandwidth floor as a suffix, empty
// when the operator set none.
func fabricBandwidthNote(gbps int32) string {
	if gbps <= 0 {
		return ""
	}
	return fmt.Sprintf(" >=%d Gbps", gbps)
}
