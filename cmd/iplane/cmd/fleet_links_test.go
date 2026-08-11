package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

func node(idx int32, cards int32, ic *provisionerv1.InterconnectHealth) *provisionerv1.EngineNode {
	return &provisionerv1.EngineNode{NodeIndex: idx, GpuCount: cards, Interconnect: ic}
}

func reading(total, up int32) *provisionerv1.InterconnectHealth {
	return &provisionerv1.InterconnectHealth{Available: true, LinksTotal: total, LinksUp: up}
}

func TestFleetLinkLabel(t *testing.T) {
	cases := []struct {
		name string
		span []*provisionerv1.EngineNode
		want string
	}{
		{
			name: "no sensor reads as no reading, not as a fault",
			span: []*provisionerv1.EngineNode{node(0, 1, nil)},
			want: "-",
		},
		{
			name: "explicitly unavailable is also no reading",
			span: []*provisionerv1.EngineNode{node(0, 1, &provisionerv1.InterconnectHealth{Available: false})},
			want: "-",
		},
		{
			name: "healthy board",
			span: []*provisionerv1.EngineNode{node(0, 4, reading(8, 8))},
			want: "8/8",
		},
		{
			name: "one link down",
			span: []*provisionerv1.EngineNode{node(0, 4, reading(8, 7))},
			want: "7/8",
		},
		{
			name: "summed across a span",
			span: []*provisionerv1.EngineNode{node(0, 4, reading(8, 8)), node(1, 4, reading(8, 6))},
			want: "14/16",
		},
		{
			name: "one node reporting is enough to show a reading",
			span: []*provisionerv1.EngineNode{node(0, 4, reading(8, 7)), node(1, 4, nil)},
			want: "7/8",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := fleetLinkLabel(&provisionerv1.Engine{Span: tc.span})
			if got != tc.want {
				t.Errorf("= %q, want %q", got, tc.want)
			}
		})
	}
}

// A board that genuinely reports zero links up is an impairment; a board with
// no sensor is not. The JSON consumer must be able to tell them apart, which
// is why links_known is carried separately from the counts.
func TestFleetJSONDistinguishesNoSensorFromZeroUp(t *testing.T) {
	var buf bytes.Buffer
	err := renderFleetJSON(&buf, []*provisionerv1.Engine{
		{Id: "no-sensor", Span: []*provisionerv1.EngineNode{node(0, 1, nil)}},
		{Id: "all-down", Span: []*provisionerv1.EngineNode{node(0, 4, reading(8, 0))}},
	})
	if err != nil {
		t.Fatalf("renderFleetJSON: %v", err)
	}

	var rows []fleetRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	byID := map[string]fleetRow{}
	for _, r := range rows {
		byID[r.Member] = r
	}

	if byID["no-sensor"].LinksKnown {
		t.Error("a member with no sensor reported links_known=true")
	}
	if !byID["all-down"].LinksKnown {
		t.Error("a member reporting zero links up was indistinguishable from one with no sensor")
	}
	if got := byID["all-down"].LinksTotal; got != 8 {
		t.Errorf("all-down links_total = %d, want 8", got)
	}
}

func TestFleetTableCarriesTheLinksColumn(t *testing.T) {
	var buf bytes.Buffer
	err := renderFleetTable(&buf, []*provisionerv1.Engine{
		{
			Id:    "m1",
			Model: "mock-model",
			State: provisionerv1.EngineState_ENGINE_STATE_SERVING_DEGRADED,
			Span:  []*provisionerv1.EngineNode{node(0, 4, reading(8, 7))},
		},
	}, time.Now())
	if err != nil {
		t.Fatalf("renderFleetTable: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "LINKS") {
		t.Errorf("no LINKS header; the reading is invisible to an operator:\n%s", out)
	}
	if !strings.Contains(out, "7/8") {
		t.Errorf("link reading missing from the row:\n%s", out)
	}
	// The state label and the reading have to agree, or the table tells two
	// different stories about the same member.
	if !strings.Contains(out, "serving, link down") {
		t.Errorf("degraded state not rendered alongside the reading:\n%s", out)
	}
}
