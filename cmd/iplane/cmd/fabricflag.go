package cmd

import (
	"fmt"
	"sort"
	"strings"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// fabricAliases maps every accepted --fabric token onto a FabricScope.
//
// The canonical vocabulary is the property one (none / intra-node /
// inter-node) because that is what the proto carries and what survives a
// non-NVIDIA fleet. The vendor tokens are kept as aliases rather than dropped
// because operators, vendor docs, and the book all say "NVLink" and "RDMA",
// and a CLI that refuses the words its users know is just friction. Sugar at
// the human edge, properties in the contract.
var fabricAliases = map[string]provisionerv1.FabricScope{
	"none":       provisionerv1.FabricScope_FABRIC_SCOPE_NONE,
	"intra-node": provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	"inter-node": provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE,

	// Vendor aliases. Deliberately many-to-one: nvlink, nvswitch and xgmi are
	// different technologies that answer the same question.
	"nvlink":     provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	"nvswitch":   provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	"xgmi":       provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE,
	"rdma":       provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE,
	"infiniband": provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE,
	"roce":       provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE,
	"efa":        provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE,
}

// parseFabricScope resolves a --fabric value. An empty string means the
// operator expressed no preference and yields UNSPECIFIED, which every
// candidate satisfies (the Ch 6-9 default, and it must stay free).
func parseFabricScope(v string) (provisionerv1.FabricScope, error) {
	token := strings.ToLower(strings.TrimSpace(v))
	if token == "" {
		return provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED, nil
	}
	if scope, ok := fabricAliases[token]; ok {
		return scope, nil
	}
	known := make([]string, 0, len(fabricAliases))
	for k := range fabricAliases {
		known = append(known, k)
	}
	sort.Strings(known)
	return provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED,
		fmt.Errorf("unknown --fabric %q (known: %s)", v, strings.Join(known, ", "))
}

// fabricFlagUsage is the shared help text. Kept in one place so the two
// commands that take the flag cannot drift apart.
const fabricFlagUsage = `required GPU interconnect: none | intra-node | inter-node ` +
	`(aliases: nvlink, nvswitch, xgmi -> intra-node; rdma, infiniband, roce, efa -> inter-node). ` +
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
