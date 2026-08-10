package engineagent

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// fakeRegistrar records what the agent sent and replies with a canned lease.
type fakeRegistrar struct {
	mu   sync.Mutex
	got  []*provisionerv1.Engine
	err  error
	seen int32 // lease_seconds to return; 0 omits the field
}

func (f *fakeRegistrar) RegisterEngine(_ context.Context, req *connect.Request[provisionerv1.RegisterEngineRequest]) (*connect.Response[provisionerv1.RegisterEngineResponse], error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.got = append(f.got, req.Msg.GetEngine())
	return connect.NewResponse(&provisionerv1.RegisterEngineResponse{
		Engine:       req.Msg.GetEngine(),
		LeaseSeconds: f.seen,
	}), nil
}

func (f *fakeRegistrar) sent() []*provisionerv1.Engine {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*provisionerv1.Engine(nil), f.got...)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testIdentity() Identity {
	return Identity{
		EngineID:     "eng-1",
		DeploymentID: "dep-1",
		Model:        "meta-llama/Llama-3.1-8B",
		Endpoint:     "https://pod-8000.proxy.runpod.net",
		Provider:     "runpod",
		HostID:       "machine-abc",
		NodeIndex:    2,
	}
}

func TestNewRequiresEngineID(t *testing.T) {
	if _, err := New(&fakeRegistrar{}, Identity{}); err == nil {
		t.Fatal("want error when engine id is empty, got nil")
	}
}

// The whole reason the registry exists next to the /health poller is that a
// probe cannot see a group that has not assembled. The agent is inside the
// box, so it can.
func TestReportsAssemblingUntilProbePasses(t *testing.T) {
	var ready bool
	f := &fakeRegistrar{}
	a, err := New(f, testIdentity(),
		WithProbe(func(context.Context) Readiness {
			if ready {
				return Ready
			}
			return NotReady
		}),
		WithLogger(quietLogger()))
	if err != nil {
		t.Fatal(err)
	}

	a.registerOnce(t.Context())
	ready = true
	a.registerOnce(t.Context())

	sent := f.sent()
	if len(sent) != 2 {
		t.Fatalf("want 2 registrations, got %d", len(sent))
	}
	if got := sent[0].GetState(); got != provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING {
		t.Errorf("first registration state = %v, want ASSEMBLING", got)
	}
	if got := sent[1].GetState(); got != provisionerv1.EngineState_ENGINE_STATE_SERVING {
		t.Errorf("second registration state = %v, want SERVING", got)
	}
}

// State is read fresh each tick rather than latched, so an engine that falls
// over after assembling stops claiming a readiness it no longer has.
func TestStateIsNotLatchedOnceServing(t *testing.T) {
	ready := true
	f := &fakeRegistrar{}
	a, _ := New(f, testIdentity(),
		WithProbe(func(context.Context) Readiness {
			if ready {
				return Ready
			}
			return NotReady
		}),
		WithLogger(quietLogger()))

	a.registerOnce(t.Context())
	ready = false
	a.registerOnce(t.Context())

	sent := f.sent()
	if got := sent[1].GetState(); got != provisionerv1.EngineState_ENGINE_STATE_ASSEMBLING {
		t.Errorf("state after probe regressed = %v, want ASSEMBLING", got)
	}
}

// No probe means no health endpoint to consult, which is the honest default
// for an engine that does not expose one.
func TestNoProbeReportsServing(t *testing.T) {
	f := &fakeRegistrar{}
	a, _ := New(f, testIdentity(), WithLogger(quietLogger()))

	a.registerOnce(t.Context())

	if got := f.sent()[0].GetState(); got != provisionerv1.EngineState_ENGINE_STATE_SERVING {
		t.Errorf("state = %v, want SERVING when no probe is configured", got)
	}
}

// The injected identity has to survive into the span, because issue 214's
// failure attribution reads exactly these fields.
func TestInjectedIdentityLandsInTheSpan(t *testing.T) {
	f := &fakeRegistrar{}
	a, _ := New(f, testIdentity(), WithCards(4), WithLogger(quietLogger()))

	a.registerOnce(t.Context())

	e := f.sent()[0]
	if e.GetId() != "eng-1" || e.GetDeploymentId() != "dep-1" {
		t.Errorf("id/deployment = %q/%q, want eng-1/dep-1", e.GetId(), e.GetDeploymentId())
	}
	if e.GetModel() != "meta-llama/Llama-3.1-8B" {
		t.Errorf("model = %q", e.GetModel())
	}
	if e.GetEndpoint() != "https://pod-8000.proxy.runpod.net" {
		t.Errorf("endpoint = %q", e.GetEndpoint())
	}
	span := e.GetSpan()
	if len(span) != 1 {
		t.Fatalf("want a 1-node span, got %d", len(span))
	}
	if span[0].GetHostId() != "machine-abc" || span[0].GetProvider() != "runpod" {
		t.Errorf("host/provider = %q/%q, want machine-abc/runpod",
			span[0].GetHostId(), span[0].GetProvider())
	}
	if span[0].GetNodeIndex() != 2 {
		t.Errorf("node_index = %d, want 2", span[0].GetNodeIndex())
	}
	if span[0].GetGpuCount() != 4 {
		t.Errorf("gpu_count = %d, want 4", span[0].GetGpuCount())
	}
}

// An unstamped host id is reported as empty rather than substituted with
// something readable-but-wrong (a container id). A visible gap is debuggable;
// a plausible lie is not.
func TestUnstampedHostIDStaysEmpty(t *testing.T) {
	ident := testIdentity()
	ident.HostID = ""
	f := &fakeRegistrar{}
	a, _ := New(f, ident, WithLogger(quietLogger()))

	a.registerOnce(t.Context())

	if got := f.sent()[0].GetSpan()[0].GetHostId(); got != "" {
		t.Errorf("host_id = %q, want empty when the deploy path did not stamp it", got)
	}
}

// An agent knows the node it runs on, so the default span is that one node.
// A caller that genuinely knows the whole group supplies it.
func TestWithSpanReplacesTheSelfReportedNode(t *testing.T) {
	f := &fakeRegistrar{}
	a, _ := New(f, testIdentity(),
		WithCards(1),
		WithSpan([]*provisionerv1.EngineNode{
			{HostId: "host-a", Provider: "mock", NodeIndex: 0, GpuCount: 2},
			{HostId: "host-b", Provider: "mock", NodeIndex: 1, GpuCount: 2},
		}),
		WithLogger(quietLogger()))

	a.registerOnce(t.Context())

	span := f.sent()[0].GetSpan()
	if len(span) != 2 {
		t.Fatalf("want a 2-node span, got %d", len(span))
	}
	if span[1].GetHostId() != "host-b" || span[1].GetGpuCount() != 2 {
		t.Errorf("second node = %q/%d, want host-b/2", span[1].GetHostId(), span[1].GetGpuCount())
	}
}

func TestEmptySpanFallsBackToTheSelfReportedNode(t *testing.T) {
	f := &fakeRegistrar{}
	a, _ := New(f, testIdentity(), WithCards(8),
		WithSpan(nil), WithLogger(quietLogger()))

	a.registerOnce(t.Context())

	span := f.sent()[0].GetSpan()
	if len(span) != 1 || span[0].GetHostId() != "machine-abc" || span[0].GetGpuCount() != 8 {
		t.Errorf("span = %+v, want the single self-reported node", span)
	}
}

// The control plane owns the cadence so detection latency is tunable in one
// place. The agent divides rather than being handed an interval, so an older
// control plane still yields a sane number.
func TestAdoptsLeaseCadenceFromTheControlPlane(t *testing.T) {
	f := &fakeRegistrar{seen: 30}
	a, _ := New(f, testIdentity(), WithLogger(quietLogger()))

	a.registerOnce(t.Context())

	if want := 10 * time.Second; a.interval != want {
		t.Errorf("interval = %v, want %v (30s lease / %d)", a.interval, want, RenewDivisor)
	}
}

func TestKeepsPriorCadenceWhenLeaseIsAbsent(t *testing.T) {
	f := &fakeRegistrar{seen: 0}
	a, _ := New(f, testIdentity(), WithInterval(7*time.Second), WithLogger(quietLogger()))

	a.registerOnce(t.Context())

	if a.interval != 7*time.Second {
		t.Errorf("interval = %v, want the configured 7s left alone", a.interval)
	}
}

// A control plane that is briefly unreachable must not stop a serving engine.
// The lease already encodes what happens if the outage outlasts it.
func TestRegistrationFailureIsNotFatal(t *testing.T) {
	f := &fakeRegistrar{err: errors.New("connection refused")}
	a, _ := New(f, testIdentity(), WithInterval(time.Millisecond), WithLogger(quietLogger()))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel; a failing registration must not wedge the loop")
	}
}

func TestRunStopsOnContextCancel(t *testing.T) {
	f := &fakeRegistrar{seen: 30}
	a, _ := New(f, testIdentity(), WithInterval(time.Millisecond), WithLogger(quietLogger()))

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		a.Run(ctx)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if len(f.sent()) == 0 {
		t.Error("want at least one registration before cancel")
	}
}

// LOST is the control plane's conclusion from an expired lease. The registry
// rejects an engine claiming it, so the agent must never produce it.
func TestAgentNeverReportsLost(t *testing.T) {
	f := &fakeRegistrar{}
	a, _ := New(f, testIdentity(),
		WithProbe(func(context.Context) Readiness { return NotReady }),
		WithLogger(quietLogger()))

	a.registerOnce(t.Context())

	if got := f.sent()[0].GetState(); got == provisionerv1.EngineState_ENGINE_STATE_LOST {
		t.Error("agent reported LOST about itself; that state is the control plane's")
	}
}

func TestHTTPProbe(t *testing.T) {
	var code int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	defer srv.Close()

	probe := HTTPProbe(srv.URL, time.Second)

	for _, tc := range []struct {
		status int
		want   Readiness
	}{
		{http.StatusOK, Ready},
		{http.StatusNoContent, Ready},
		{http.StatusServiceUnavailable, NotReady},
		{http.StatusInternalServerError, NotReady},
	} {
		code = tc.status
		if got := probe(t.Context()); got != tc.want {
			t.Errorf("probe on %d = %v, want %v", tc.status, got, tc.want)
		}
	}
}

// An engine still coming up refuses the connection outright. That is
// ASSEMBLING, not an error to escalate.
func TestHTTPProbeUnreachableIsNotServing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	if got := HTTPProbe(url, 100*time.Millisecond)(t.Context()); got != NotReady {
		t.Errorf("probe against a closed listener = %v, want not-ready", got)
	}
}
