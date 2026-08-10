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

		{"  INTRA-NODE  ", provisionerv1.FabricScope_FABRIC_SCOPE_INTRA_NODE},
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

// A vendor name must be REFUSED, not quietly widened. --fabric nvlink
// resolving to intra-node would hand a mixed fleet an AMD xGMI host under a
// token that reads as a promise of NVLink specifically.
func TestParseFabricScopeRefusesVendorNames(t *testing.T) {
	for _, in := range []string{"nvlink", "nvswitch", "xgmi", "rdma", "infiniband", "roce", "efa"} {
		got, err := parseFabricScope(in)
		if err == nil {
			t.Errorf("parseFabricScope(%q) = %v, want a refusal; vendor names are not scopes", in, got)
			continue
		}
		if got != provisionerv1.FabricScope_FABRIC_SCOPE_UNSPECIFIED {
			t.Errorf("parseFabricScope(%q) returned scope %v alongside its error", in, got)
		}
	}
}

// The refusal has to teach the right word, or it is just friction. Operators
// and the chapter both reach for "NVLink" first.
func TestVendorNameErrorNamesTheRightToken(t *testing.T) {
	tests := map[string]string{
		"nvlink":     "intra-node",
		"xgmi":       "intra-node",
		"rdma":       "inter-node",
		"infiniband": "inter-node",
	}
	for in, want := range tests {
		_, err := parseFabricScope(in)
		if err == nil {
			t.Fatalf("parseFabricScope(%q) did not error", in)
		}
		if !strings.Contains(err.Error(), "--fabric "+want) {
			t.Errorf("parseFabricScope(%q) error %q should point at --fabric %s", in, err, want)
		}
	}
}

// Every vendor hint must name a real scope, or the redirect sends operators
// at a token the parser then rejects.
func TestVendorHintsPointAtRealScopes(t *testing.T) {
	for name, scope := range vendorHints {
		if _, ok := fabricScopes[scope]; !ok {
			t.Errorf("hint %q redirects to %q, which is not an accepted --fabric value", name, scope)
		}
	}
}
