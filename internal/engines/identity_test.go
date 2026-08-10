package engines

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// locateStub records lookups and returns a canned identity.
type locateStub struct {
	id     NodeIdentity
	found  bool
	err    error
	calls  int
	lastID string
}

func (l *locateStub) LocateEngine(_ context.Context, engineID string) (NodeIdentity, bool, error) {
	l.calls++
	l.lastID = engineID
	return l.id, l.found, l.err
}

func newAdapterWithLocator(t *testing.T, l Locator) *ConnectAdapter {
	t.Helper()
	return NewConnectAdapter(New(newMemStore()), WithLocator(l))
}

func register(t *testing.T, a *ConnectAdapter, e *provisionerv1.Engine) *provisionerv1.Engine {
	t.Helper()
	resp, err := a.RegisterEngine(t.Context(), connect.NewRequest(
		&provisionerv1.RegisterEngineRequest{Engine: e}))
	if err != nil {
		t.Fatalf("RegisterEngine: %v", err)
	}
	return resp.Msg.GetEngine()
}

// The core of the design: the agent reports what it can read, the control
// plane fills what only it knows.
func TestEnrichFillsWhatTheBoxCannotKnow(t *testing.T) {
	loc := &locateStub{
		id:    NodeIdentity{HostID: "machine-77", Provider: "runpod", Endpoint: "https://pod-8000.proxy.runpod.net"},
		found: true,
	}
	a := newAdapterWithLocator(t, loc)

	got := register(t, a, &provisionerv1.Engine{
		Id:    "dep-1-r0",
		Model: "m",
		State: provisionerv1.EngineState_ENGINE_STATE_SERVING,
		Span:  []*provisionerv1.EngineNode{{GpuCount: 4}},
	})

	if got.GetEndpoint() != "https://pod-8000.proxy.runpod.net" {
		t.Errorf("endpoint = %q, want the located one", got.GetEndpoint())
	}
	if got.GetSpan()[0].GetHostId() != "machine-77" {
		t.Errorf("host id = %q, want machine-77", got.GetSpan()[0].GetHostId())
	}
	if got.GetSpan()[0].GetProvider() != "runpod" {
		t.Errorf("provider = %q, want runpod", got.GetSpan()[0].GetProvider())
	}
	// What the agent did read is untouched.
	if got.GetSpan()[0].GetGpuCount() != 4 {
		t.Errorf("gpu count = %d, want the agent's 4", got.GetSpan()[0].GetGpuCount())
	}
	if loc.lastID != "dep-1-r0" {
		t.Errorf("looked up %q, want the engine id", loc.lastID)
	}
}

// A reported value is a better source than our record of what we rented, and
// overwriting it would hide a real disagreement between the two.
func TestReportedValuesAreNotOverwritten(t *testing.T) {
	loc := &locateStub{
		id:    NodeIdentity{HostID: "machine-from-state", Provider: "runpod", Endpoint: "https://from-state"},
		found: true,
	}
	a := newAdapterWithLocator(t, loc)

	got := register(t, a, &provisionerv1.Engine{
		Id:       "dep-1-r0",
		Endpoint: "https://reported-by-agent",
		State:    provisionerv1.EngineState_ENGINE_STATE_SERVING,
		Span: []*provisionerv1.EngineNode{{
			HostId: "machine-from-agent", Provider: "vast", GpuCount: 2,
		}},
	})

	if got.GetEndpoint() != "https://reported-by-agent" {
		t.Errorf("endpoint = %q, want the agent's", got.GetEndpoint())
	}
	if got.GetSpan()[0].GetHostId() != "machine-from-agent" {
		t.Errorf("host id = %q, want the agent's", got.GetSpan()[0].GetHostId())
	}
	if got.GetSpan()[0].GetProvider() != "vast" {
		t.Errorf("provider = %q, want the agent's", got.GetSpan()[0].GetProvider())
	}
	if loc.calls != 0 {
		t.Errorf("locator called %d times for a complete registration; renewals must not read state", loc.calls)
	}
}

// A member that registers with a blank host id is visible and debuggable. One
// that fails to register because a lookup errored is invisible, which is the
// failure the channel exists to prevent.
func TestEnrichmentFailureDoesNotFailRegistration(t *testing.T) {
	a := newAdapterWithLocator(t, &locateStub{err: errors.New("state file unreadable")})

	got := register(t, a, &provisionerv1.Engine{
		Id:    "dep-1-r0",
		State: provisionerv1.EngineState_ENGINE_STATE_SERVING,
		Span:  []*provisionerv1.EngineNode{{GpuCount: 1}},
	})

	if got.GetId() != "dep-1-r0" {
		t.Fatal("registration did not survive an enrichment failure")
	}
	if got.GetSpan()[0].GetHostId() != "" {
		t.Error("host id invented despite the lookup failing")
	}
}

// An engine that registered without iplane provisioning it is a legitimate
// case, not an error. It reports what it knows and nothing is filled in.
func TestUnprovisionedEngineIsStoredAsReported(t *testing.T) {
	a := newAdapterWithLocator(t, &locateStub{found: false})

	got := register(t, a, &provisionerv1.Engine{
		Id:       "operator-run-engine",
		Endpoint: "https://my-own-engine.example.com",
		State:    provisionerv1.EngineState_ENGINE_STATE_SERVING,
	})

	if got.GetEndpoint() != "https://my-own-engine.example.com" {
		t.Errorf("endpoint = %q", got.GetEndpoint())
	}
	if len(got.GetSpan()) != 0 {
		t.Errorf("span = %v, want none fabricated for an engine we did not provision", got.GetSpan())
	}
}

// A multi-node engine reports its own composition. Completing one slot's
// record across a group would invent membership.
func TestMultiNodeSpanIsNotEnriched(t *testing.T) {
	loc := &locateStub{
		id:    NodeIdentity{HostID: "machine-77", Provider: "runpod", Endpoint: "https://located"},
		found: true,
	}
	a := newAdapterWithLocator(t, loc)

	got := register(t, a, &provisionerv1.Engine{
		Id:    "dep-1-r0",
		State: provisionerv1.EngineState_ENGINE_STATE_SERVING,
		Span: []*provisionerv1.EngineNode{
			{HostId: "node-a", NodeIndex: 0, GpuCount: 4},
			{NodeIndex: 1, GpuCount: 4},
		},
	})

	if loc.calls != 0 {
		t.Errorf("locator called %d times for a multi-node span", loc.calls)
	}
	if got.GetSpan()[1].GetHostId() != "" {
		t.Error("filled a second node's host id from one slot's record")
	}
}

// An agent that reports no span at all still gets a node, because the
// control plane knows there is one.
func TestEmptySpanGainsTheLocatedNode(t *testing.T) {
	a := newAdapterWithLocator(t, &locateStub{
		id:    NodeIdentity{HostID: "machine-77", Provider: "runpod"},
		found: true,
	})

	got := register(t, a, &provisionerv1.Engine{
		Id:    "dep-1-r0",
		State: provisionerv1.EngineState_ENGINE_STATE_SERVING,
	})

	if len(got.GetSpan()) != 1 {
		t.Fatalf("span = %v, want one located node", got.GetSpan())
	}
	if got.GetSpan()[0].GetHostId() != "machine-77" {
		t.Errorf("host id = %q", got.GetSpan()[0].GetHostId())
	}
}

// Without a locator the registry stores exactly what was reported, which is
// the right behaviour for a control plane that provisioned nothing.
func TestNoLocatorStoresAsReported(t *testing.T) {
	a := NewConnectAdapter(New(newMemStore()))

	got := register(t, a, &provisionerv1.Engine{
		Id:    "dep-1-r0",
		State: provisionerv1.EngineState_ENGINE_STATE_SERVING,
		Span:  []*provisionerv1.EngineNode{{GpuCount: 1}},
	})

	if got.GetSpan()[0].GetHostId() != "" || got.GetEndpoint() != "" {
		t.Error("fields appeared with no locator wired")
	}
}
