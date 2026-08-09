package cmd

import (
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

func TestParseFabricScope(t *testing.T) {
	tests := []struct {
		in   string
		want provisionerv1.FabricScope
	}{
		{"", provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED},
		{"none", provisionerv1.FabricScope_FABRIC_SCOPE_NONE},
		{"intra-node", provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE},
		{"inter-node", provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE},

		// Vendor aliases collapse onto the property, which is the point.
		{"nvlink", provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE},
		{"nvswitch", provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE},
		{"xgmi", provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE},
		{"rdma", provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE},
		{"infiniband", provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE},
		{"efa", provisionerv1.FabricScope_FABRIC_SCOPE_INTER_NODE},

		{"  NVLink  ", provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE},
		{"INTRA-NODE", provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseFabricScope(tt.in)
			if err != nil {
				t.Fatalf("parseFabricScope(%q) errored: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("parseFabricScope(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseFabricScopeRejectsUnknown(t *testing.T) {
	for _, in := range []string{"nvlink4", "fast", "yes", "intra node"} {
		got, err := parseFabricScope(in)
		if err == nil {
			t.Errorf("parseFabricScope(%q) = %v, want an error", in, got)
			continue
		}
		if !strings.Contains(err.Error(), "known:") {
			t.Errorf("parseFabricScope(%q) error %q should list the accepted values", in, err)
		}
	}
}
