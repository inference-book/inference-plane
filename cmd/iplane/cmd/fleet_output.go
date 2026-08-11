package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/engines"
)

// fleetStateLabel renders an engine state for operators.
//
// The labels matter more than usual here. "serving, link down" is deliberately
// not a single word and deliberately not an alarm word: the member is
// assembled, returning correct tokens, and delivering a fraction of the
// throughput being paid for. Calling it "degraded" invites reading it as
// broken; calling it "serving" hides the whole reason the state exists. And
// "assembling" must not read as a failure, because it is the ordinary first
// few seconds of every distributed engine's life.
func fleetStateLabel(s provisionerv1.EngineState) string {
	switch s {
	case provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING:
		return "assembling"
	case provisionerv1.EngineState_ENGINE_STATE_SERVING:
		return "serving"
	case provisionerv1.EngineState_ENGINE_STATE_SERVING_DEGRADED:
		return "serving, link down"
	case provisionerv1.EngineState_ENGINE_STATE_DRAINING:
		return "draining"
	case provisionerv1.EngineState_ENGINE_STATE_LOST:
		return "lost"
	default:
		return "unknown"
	}
}

// fleetSpanLabel renders the span column: cards first, since that is what the
// operator sized the model against, then nodes.
//
// A single-card engine reads "1c/1n" rather than being blanked, so the column
// means the same thing on every row. The column existing at all is the
// chapter's point: a distributed member is the same kind of object as a
// single-card one, differing only here.
func fleetSpanLabel(e *provisionerv1.Engine) string {
	nodes := len(e.GetSpan())
	cards := engines.TotalCards(e)
	if nodes == 0 {
		return "-"
	}
	return fmt.Sprintf("%dc/%dn", cards, nodes)
}

// fleetLinkLabel renders link health across the member's span.
//
// Three outcomes, and keeping them distinct is the whole point of the column:
//
//	"-"     no node reported a reading. A PCIe-only pool, or one whose
//	        provider hides the NVIDIA tooling in the container. NOT a fault.
//	"8/8"   every reported link is up.
//	"6/8"   two links are down somewhere in the span, which is what a member
//	        sitting in SERVING_DEGRADED looks like from here.
//
// Summed across the span rather than shown per node, because the fleet list is
// one line per member. Which node is impaired is a question for describe; this
// column exists to make "something in here is degraded" visible at a glance,
// since the endpoint answers and the tokens are correct either way.
func fleetLinkLabel(e *provisionerv1.Engine) string {
	total, up, known := fleetLinks(e)
	if !known {
		return "-"
	}
	return fmt.Sprintf("%d/%d", up, total)
}

// fleetLinks sums link health across a span. known is false when no node
// reported a reading, and it is carried separately from the counts rather than
// being inferred from total==0 so the JSON consumer can tell "no sensor" from
// "a board that reports zero links". Collapsing those is how a PCIe pool ends
// up looking like an impaired NVLink pool.
func fleetLinks(e *provisionerv1.Engine) (total, up int32, known bool) {
	for _, n := range e.GetSpan() {
		ic := n.GetInterconnect()
		if ic == nil || !ic.GetAvailable() {
			continue
		}
		known = true
		total += ic.GetLinksTotal()
		up += ic.GetLinksUp()
	}
	return total, up, known
}

// fleetAge renders how long a member has been registered, coarsely. Operators
// reading a fleet list want "is this new or has it been up a while", not
// millisecond precision.
func fleetAge(e *provisionerv1.Engine, now time.Time) string {
	ts := e.GetRegisteredAt()
	if ts == nil {
		return "-"
	}
	d := now.Sub(ts.AsTime())
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd%dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

// renderFleet writes the member list in the requested format.
func renderFleet(w io.Writer, format string, es []*provisionerv1.Engine) error {
	switch format {
	case "json":
		return renderFleetJSON(w, es)
	case "table":
		return renderFleetTable(w, es, time.Now())
	default:
		return fmt.Errorf("%w: %q", errUnknownFleetOutput, format)
	}
}

// fleetRow is the JSON shape. Hand-rolled rather than protojson so the span
// arrives pre-summed and the state arrives as the operator-facing label,
// which is what a script consuming this actually wants.
type fleetRow struct {
	Member       string `json:"member"`
	Model        string `json:"model"`
	Endpoint     string `json:"endpoint"`
	Cards        int32  `json:"cards"`
	Nodes        int    `json:"nodes"`
	LinksTotal   int32  `json:"links_total,omitempty"`
	LinksUp      int32  `json:"links_up,omitempty"`
	LinksKnown   bool   `json:"links_known"`
	State        string `json:"state"`
	DeploymentID string `json:"deployment_id,omitempty"`
	RegisteredAt string `json:"registered_at,omitempty"`
	LastSeenAt   string `json:"last_seen_at,omitempty"`
}

func renderFleetJSON(w io.Writer, es []*provisionerv1.Engine) error {
	rows := make([]fleetRow, 0, len(es))
	for _, e := range es {
		row := fleetRow{
			Member:       e.GetId(),
			Model:        e.GetModel(),
			Endpoint:     e.GetEndpoint(),
			Cards:        engines.TotalCards(e),
			Nodes:        len(e.GetSpan()),
			State:        fleetStateLabel(e.GetState()),
			DeploymentID: e.GetDeploymentId(),
		}
		row.LinksTotal, row.LinksUp, row.LinksKnown = fleetLinks(e)
		if ts := e.GetRegisteredAt(); ts != nil {
			row.RegisteredAt = ts.AsTime().UTC().Format(time.RFC3339)
		}
		if ts := e.GetLastSeenAt(); ts != nil {
			row.LastSeenAt = ts.AsTime().UTC().Format(time.RFC3339)
		}
		rows = append(rows, row)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

func renderFleetTable(w io.Writer, es []*provisionerv1.Engine, now time.Time) error {
	if len(es) == 0 {
		_, err := fmt.Fprintln(w, "no engines have registered")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "MEMBER\tMODEL\tSPAN\tLINKS\tSTATE\tAGE"); err != nil {
		return err
	}
	for _, e := range es {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			e.GetId(),
			orDash(e.GetModel()),
			fleetSpanLabel(e),
			fleetLinkLabel(e),
			fleetStateLabel(e.GetState()),
			fleetAge(e, now),
		); err != nil {
			return err
		}
	}
	return tw.Flush()
}
