package engineagent

import (
	"context"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Real `nvidia-smi nvlink -s` output from an NVLink board, one link down.
const nvlinkPartialOutput = `GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-f3aa01f8-0000-0000-0000-000000000000)
	 Link 0: 25.781 GB/s
	 Link 1: 25.781 GB/s
	 Link 2: <inactive>
	 Link 3: 25.781 GB/s`

const nvlinkHealthyOutput = `GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-f3aa01f8-0000-0000-0000-000000000000)
	 Link 0: 25.781 GB/s
	 Link 1: 25.781 GB/s`

// A board with no NVLink at all. The distinction from "all links down" is the
// whole point of the available flag.
const nvlinkUnsupportedOutput = `GPU 0: NVIDIA GeForce RTX 3090 (UUID: GPU-6c620167-0000-0000-0000-000000000000)
	 (Not supported)`

func TestParseNVLinkCountsUpAndDownLinks(t *testing.T) {
	got := parseNVLinkStatus(nvlinkPartialOutput)

	if !got.GetAvailable() {
		t.Fatal("a board reporting links was read as having no sensor")
	}
	if got.GetLinksTotal() != 4 {
		t.Errorf("links_total = %d, want 4", got.GetLinksTotal())
	}
	if got.GetLinksUp() != 3 {
		t.Errorf("links_up = %d, want 3 (<inactive> is down)", got.GetLinksUp())
	}
}

func TestParseNVLinkHealthyBoard(t *testing.T) {
	got := parseNVLinkStatus(nvlinkHealthyOutput)
	if got.GetLinksTotal() != 2 || got.GetLinksUp() != 2 {
		t.Errorf("= %d/%d up/total, want 2/2", got.GetLinksUp(), got.GetLinksTotal())
	}
	if InterconnectImpaired(got) {
		t.Error("a fully-up board was reported impaired")
	}
}

// The failure that would mark every PCIe pool degraded. "No links reported"
// must be absence of a reading, never zero links up.
func TestParseNVLinkNoLinksIsNoReadingNotZeroUp(t *testing.T) {
	for name, out := range map[string]string{
		"not supported": nvlinkUnsupportedOutput,
		"empty":         "",
		"header only":   "GPU 0: NVIDIA GeForce RTX 3090 (UUID: GPU-abc)",
	} {
		t.Run(name, func(t *testing.T) {
			got := parseNVLinkStatus(out)
			if got.GetAvailable() {
				t.Errorf("reported a reading for a board with no links: %+v", got)
			}
			if InterconnectImpaired(got) {
				t.Error("absence of a reading was treated as an impairment")
			}
		})
	}
}

// Positive evidence only. Matching on the absence of "<inactive>" would count
// "(Not supported)" and any future status string as a healthy link, which
// fails open on exactly the reading this exists to catch.
func TestParseNVLinkRequiresABandwidthToCallALinkUp(t *testing.T) {
	out := `GPU 0: NVIDIA A100-SXM4-40GB (UUID: GPU-abc)
	 Link 0: <inactive>
	 Link 1: some future status nobody has seen`

	got := parseNVLinkStatus(out)

	if got.GetLinksTotal() != 2 {
		t.Fatalf("links_total = %d, want 2", got.GetLinksTotal())
	}
	if got.GetLinksUp() != 0 {
		t.Errorf("links_up = %d, want 0; a link with no bandwidth figure is not evidence of a working link", got.GetLinksUp())
	}
}

func TestInterconnectImpairedThreshold(t *testing.T) {
	cases := []struct {
		name string
		in   *provisionerv1.InterconnectHealth
		want bool
	}{
		{"nil is not an impairment", nil, false},
		{"no reading is not an impairment", &provisionerv1.InterconnectHealth{Available: false}, false},
		{"all up", &provisionerv1.InterconnectHealth{Available: true, LinksTotal: 8, LinksUp: 8}, false},
		{"one down", &provisionerv1.InterconnectHealth{Available: true, LinksTotal: 8, LinksUp: 7}, true},
		{"all down", &provisionerv1.InterconnectHealth{Available: true, LinksTotal: 8, LinksUp: 0}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := InterconnectImpaired(tc.in); got != tc.want {
				t.Errorf("= %v, want %v", got, tc.want)
			}
		})
	}
}

// The composition that turns a reading into a reported state. Not ready must
// still win: a group that has not formed is not a degraded group.
func TestInterconnectProbeComposesWithReadiness(t *testing.T) {
	oneDown := func(context.Context) *provisionerv1.InterconnectHealth {
		return &provisionerv1.InterconnectHealth{Available: true, LinksTotal: 8, LinksUp: 7}
	}
	impaired := InterconnectProbe(oneDown)

	serving := AnyDegraded(func(context.Context) Readiness { return Ready }, impaired)
	if got := serving(context.Background()); got != Degraded {
		t.Errorf("serving engine with a downed link = %v, want Degraded", got)
	}

	assembling := AnyDegraded(func(context.Context) Readiness { return NotReady }, impaired)
	if got := assembling(context.Background()); got != NotReady {
		t.Errorf("assembling engine with a downed link = %v, want NotReady; assembly is not a fault", got)
	}
}

// A sensor-less box must report SERVING, never Degraded, or every pool we
// cannot measure looks broken.
func TestInterconnectProbeWithoutASensorReportsReady(t *testing.T) {
	none := func(context.Context) *provisionerv1.InterconnectHealth {
		return &provisionerv1.InterconnectHealth{Available: false}
	}
	probe := AnyDegraded(func(context.Context) Readiness { return Ready }, InterconnectProbe(none))

	if got := probe(context.Background()); got != Ready {
		t.Errorf("= %v, want Ready; absence of a reading is not evidence of a fault", got)
	}
}

// The agent stamps the reading onto the node it runs on, and only that one:
// it can see its own board and nothing else.
func TestSnapshotStampsInterconnectOnItsOwnNode(t *testing.T) {
	span := []*provisionerv1.EngineNode{
		{HostId: "h0", NodeIndex: 0, GpuCount: 4},
		{HostId: "h1", NodeIndex: 1, GpuCount: 4},
	}
	a := &Agent{
		ident: Identity{EngineID: "e", NodeIndex: 0},
		span:  span,
		probe: func(context.Context) Readiness { return Ready },
		interconnect: func(context.Context) *provisionerv1.InterconnectHealth {
			return &provisionerv1.InterconnectHealth{Available: true, LinksTotal: 8, LinksUp: 7}
		},
	}

	got := a.snapshot(context.Background())

	if ic := got.GetSpan()[0].GetInterconnect(); ic == nil || ic.GetLinksUp() != 7 {
		t.Errorf("own node carries no reading: %+v", got.GetSpan()[0])
	}
	if ic := got.GetSpan()[1].GetInterconnect(); ic != nil {
		t.Errorf("stamped a reading onto another node's hardware it never looked at: %+v", ic)
	}
}

// An agent built without the sensor must report exactly what it did before,
// so upgrading the binary does not start making claims about hardware.
func TestSnapshotOmitsInterconnectWhenUnset(t *testing.T) {
	a := &Agent{
		ident: Identity{EngineID: "e"},
		cards: 1,
		probe: func(context.Context) Readiness { return Ready },
	}

	got := a.snapshot(context.Background())

	if ic := got.GetSpan()[0].GetInterconnect(); ic != nil {
		t.Errorf("an agent with no sensor reported one: %+v", ic)
	}
}
