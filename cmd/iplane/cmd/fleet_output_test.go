package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var renderNow = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

func testEngine(id, model string, state provisionerv1.EngineState, nodes, cardsPerNode int32) *provisionerv1.Engine {
	span := make([]*provisionerv1.EngineNode, 0, nodes)
	for i := range nodes {
		span = append(span, &provisionerv1.EngineNode{
			HostId:    id + "-h",
			NodeIndex: i,
			GpuCount:  cardsPerNode,
		})
	}
	return &provisionerv1.Engine{
		Id:           id,
		Model:        model,
		Endpoint:     "http://127.0.0.1:9001",
		State:        state,
		Span:         span,
		RegisteredAt: timestamppb.New(renderNow.Add(-5 * time.Minute)),
	}
}

// The chapter's claim, asserted: a single-card member and a distributed one
// live in one list and differ only in the span column. A second object type,
// or a distributed member rendered specially, would break the promise that a
// distributed engine is the same kind of thing as a single-card one.
func TestFleetTableShowsBothShapesInOneList(t *testing.T) {
	var buf bytes.Buffer
	err := renderFleetTable(&buf, []*provisionerv1.Engine{
		testEngine("single-beta", "qwen-1.5b", provisionerv1.EngineState_ENGINE_STATE_SERVING, 1, 1),
		testEngine("tp4-alpha", "llama-70b", provisionerv1.EngineState_ENGINE_STATE_SERVING, 1, 4),
	}, renderNow)
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("want header + 2 rows, got %d lines:\n%s", len(lines), out)
	}
	if !strings.Contains(lines[1], "1c/1n") {
		t.Errorf("single-card member did not render a span: %q", lines[1])
	}
	if !strings.Contains(lines[2], "4c/1n") {
		t.Errorf("4-card member did not render its span: %q", lines[2])
	}
	// Both rows carry the same columns; the span is the only difference.
	for _, l := range lines[1:] {
		if !strings.Contains(l, "serving") {
			t.Errorf("row missing state: %q", l)
		}
	}
}

func TestFleetSpanLabel(t *testing.T) {
	tests := []struct {
		name  string
		nodes int32
		cards int32
		want  string
	}{
		{"single card", 1, 1, "1c/1n"},
		{"four cards one node", 1, 4, "4c/1n"},
		{"cross-node group", 2, 4, "8c/2n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := testEngine("e", "m", provisionerv1.EngineState_ENGINE_STATE_SERVING, tt.nodes, tt.cards)
			if got := fleetSpanLabel(e); got != tt.want {
				t.Errorf("fleetSpanLabel = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFleetSpanLabelWithNoSpan(t *testing.T) {
	e := &provisionerv1.Engine{Id: "e"}
	if got := fleetSpanLabel(e); got != "-" {
		t.Errorf("engine with no span rendered %q, want %q", got, "-")
	}
}

// The two states the whole registry exists for must not read as either
// healthy or failed. "assembling" is the ordinary first seconds of a
// distributed engine's life, and a member that lost a link is serving correct
// tokens while delivering a fraction of the throughput being paid for.
func TestFleetStateLabelsDoNotMislead(t *testing.T) {
	assembling := fleetStateLabel(provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING)
	if assembling != "assembling" {
		t.Errorf("assembling label = %q", assembling)
	}
	for _, bad := range []string{"down", "fail", "error", "unhealthy"} {
		if strings.Contains(assembling, bad) {
			t.Errorf("assembling label %q contains %q; it is not a failure", assembling, bad)
		}
	}

	degraded := fleetStateLabel(provisionerv1.EngineState_ENGINE_STATE_SERVING_DEGRADED)
	if !strings.Contains(degraded, "serving") {
		t.Errorf("degraded label %q should say it is still serving", degraded)
	}
	if degraded == fleetStateLabel(provisionerv1.EngineState_ENGINE_STATE_SERVING) {
		t.Error("degraded and serving render identically; the distinction is the point of the state")
	}
}

func TestFleetStateLabelCoversEveryState(t *testing.T) {
	all := []provisionerv1.EngineState{
		provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING,
		provisionerv1.EngineState_ENGINE_STATE_SERVING,
		provisionerv1.EngineState_ENGINE_STATE_SERVING_DEGRADED,
		provisionerv1.EngineState_ENGINE_STATE_DRAINING,
		provisionerv1.EngineState_ENGINE_STATE_LOST,
	}
	seen := map[string]bool{}
	for _, s := range all {
		label := fleetStateLabel(s)
		if label == "unknown" {
			t.Errorf("%v has no operator-facing label", s)
		}
		if seen[label] {
			t.Errorf("%v shares the label %q with another state", s, label)
		}
		seen[label] = true
	}
}

func TestFleetTableEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := renderFleetTable(&buf, nil, renderNow); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no engines have registered") {
		t.Errorf("empty fleet rendered %q", buf.String())
	}
}

func TestFleetJSONCarriesSummedSpan(t *testing.T) {
	var buf bytes.Buffer
	err := renderFleetJSON(&buf, []*provisionerv1.Engine{
		testEngine("tp8", "llama-70b", provisionerv1.EngineState_ENGINE_STATE_SERVING_DEGRADED, 2, 4),
	})
	if err != nil {
		t.Fatal(err)
	}
	var rows []fleetRow
	if err := json.Unmarshal(buf.Bytes(), &rows); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Cards != 8 || rows[0].Nodes != 2 {
		t.Errorf("span = %dc/%dn, want 8c/2n", rows[0].Cards, rows[0].Nodes)
	}
	if rows[0].State != "serving, link down" {
		t.Errorf("JSON state = %q; scripts should see the same label the table shows", rows[0].State)
	}
}

// LOST members are hidden by default, matching deployment list and instance
// list, and --show-lost brings them back. An operator asking why something
// disappeared needs the second behavior.
func TestFilterLostEngines(t *testing.T) {
	in := []*provisionerv1.Engine{
		testEngine("a", "m", provisionerv1.EngineState_ENGINE_STATE_SERVING, 1, 1),
		testEngine("b", "m", provisionerv1.EngineState_ENGINE_STATE_LOST, 1, 1),
		testEngine("c", "m", provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING, 1, 1),
	}
	got := filterLostEngines(in)
	if len(got) != 2 {
		t.Fatalf("filtered to %d members, want 2", len(got))
	}
	for _, e := range got {
		if e.GetState() == provisionerv1.EngineState_ENGINE_STATE_LOST {
			t.Error("a LOST member survived the default filter")
		}
		if e.GetId() == "c" && e.GetState() != provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING {
			t.Error("assembling member was altered")
		}
	}
}

// An assembling member must survive the default filter. It is easy to lump it
// in with lost as "not serving", which would hide exactly the state an
// operator is looking for while a deployment comes up.
func TestAssemblingSurvivesDefaultFilter(t *testing.T) {
	in := []*provisionerv1.Engine{
		testEngine("c", "m", provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING, 1, 4),
	}
	if got := filterLostEngines(in); len(got) != 1 {
		t.Error("assembling member was hidden by the default filter; it is not a terminal state")
	}
}

func TestFleetAge(t *testing.T) {
	tests := []struct {
		since time.Duration
		want  string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h30m"},
		{50 * time.Hour, "2d2h"},
	}
	for _, tt := range tests {
		e := &provisionerv1.Engine{RegisteredAt: timestamppb.New(renderNow.Add(-tt.since))}
		if got := fleetAge(e, renderNow); got != tt.want {
			t.Errorf("age after %v = %q, want %q", tt.since, got, tt.want)
		}
	}
}

func TestRenderFleetRejectsUnknownFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := renderFleet(&buf, "yaml", nil); err == nil {
		t.Error("unknown output format was accepted")
	}
}
