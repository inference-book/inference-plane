package engines

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

type fakeDrainer struct {
	called   []string
	released []string
	err      error
}

func (f *fakeDrainer) DrainAndDestroyDeployment(_ context.Context, deployID string, _ provisioners.DrainOptions) ([]string, error) {
	f.called = append(f.called, deployID)
	return f.released, f.err
}

func registeredEngine(t *testing.T, r *Registry, id, deployID string) {
	t.Helper()
	if _, err := r.Register(&provisionerv1.Engine{
		Id:           id,
		DeploymentId: deployID,
		State:        provisionerv1.EngineState_ENGINE_STATE_SERVING,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDrainEngineMarksDrainingAndReleases(t *testing.T) {
	r := New(newMemStore())
	registeredEngine(t, r, "tp4", "my-llama")
	d := &fakeDrainer{released: []string{"my-llama-r0", "my-llama-r1"}}
	a := NewConnectAdapter(r, WithDrainer(d))

	resp, err := a.DrainEngine(context.Background(), connect.NewRequest(
		&provisionerv1.DrainEngineRequest{EngineId: "tp4", Force: true}))
	if err != nil {
		t.Fatalf("DrainEngine: %v", err)
	}
	if got := resp.Msg.GetEngine().GetState(); got != provisionerv1.EngineState_ENGINE_STATE_DRAINING {
		t.Errorf("state = %v, want DRAINING", got)
	}
	if len(resp.Msg.GetReleasedInstanceIds()) != 2 {
		t.Errorf("released %v, want both nodes", resp.Msg.GetReleasedInstanceIds())
	}
	if len(d.called) != 1 || d.called[0] != "my-llama" {
		t.Errorf("drainer called with %v, want [my-llama]", d.called)
	}
}

// A renewal arriving mid-drain must not un-drain the member. The engine is
// still alive and still reporting SERVING, because it does not know an
// operator decided to reclaim it.
func TestRenewalDoesNotUndrain(t *testing.T) {
	r := New(newMemStore())
	registeredEngine(t, r, "tp4", "my-llama")
	if _, err := r.SetState("tp4", provisionerv1.EngineState_ENGINE_STATE_DRAINING); err != nil {
		t.Fatal(err)
	}

	renewed, err := r.Register(&provisionerv1.Engine{
		Id:           "tp4",
		DeploymentId: "my-llama",
		State:        provisionerv1.EngineState_ENGINE_STATE_SERVING,
	})
	if err != nil {
		t.Fatal(err)
	}
	if renewed.GetState() != provisionerv1.EngineState_ENGINE_STATE_DRAINING {
		t.Errorf("state = %v after renewal, want DRAINING to stick", renewed.GetState())
	}
}

// An engine iplane never provisioned has no replicas to quarantine and no
// hardware to release. Reporting success would claim work that did not happen.
func TestDrainEngineRejectsNonProvisionedMember(t *testing.T) {
	r := New(newMemStore())
	registeredEngine(t, r, "operator-run", "")
	a := NewConnectAdapter(r, WithDrainer(&fakeDrainer{}))

	_, err := a.DrainEngine(context.Background(), connect.NewRequest(
		&provisionerv1.DrainEngineRequest{EngineId: "operator-run", Force: true}))
	if err == nil {
		t.Fatal("draining an engine with no deployment reported success")
	}
	if !strings.Contains(err.Error(), "not provisioned by iplane") {
		t.Errorf("error %q should explain why there is nothing to drain", err)
	}
}

func TestDrainEngineUnknownMember(t *testing.T) {
	a := NewConnectAdapter(New(newMemStore()), WithDrainer(&fakeDrainer{}))
	_, err := a.DrainEngine(context.Background(), connect.NewRequest(
		&provisionerv1.DrainEngineRequest{EngineId: "ghost"}))
	if err == nil {
		t.Fatal("draining an unknown member reported success")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("code = %v, want NotFound", connect.CodeOf(err))
	}
}

// A timeout the transport cannot carry is rejected rather than truncated.
// Silently draining for less than asked would cut in-flight work the
// operator explicitly budgeted for -- and a severed long unary call is the
// failure iplane already ate once in Ch 9.
func TestDrainEngineRejectsTimeoutBeyondTransport(t *testing.T) {
	r := New(newMemStore())
	registeredEngine(t, r, "tp4", "my-llama")
	a := NewConnectAdapter(r, WithDrainer(&fakeDrainer{}), WithMaxDrainTimeout(30*time.Second))

	_, err := a.DrainEngine(context.Background(), connect.NewRequest(
		&provisionerv1.DrainEngineRequest{EngineId: "tp4", TimeoutSeconds: 600}))
	if err == nil {
		t.Fatal("an over-long timeout was accepted")
	}
	if !strings.Contains(err.Error(), "write_timeout_sec") {
		t.Errorf("error %q should name the setting an operator would change", err)
	}

	// --force has no wait, so the cap does not apply to it.
	if _, err := a.DrainEngine(context.Background(), connect.NewRequest(
		&provisionerv1.DrainEngineRequest{EngineId: "tp4", TimeoutSeconds: 600, Force: true})); err != nil {
		t.Errorf("--force was blocked by the wait cap: %v", err)
	}
}

// Without a drainer the RPC must say so rather than marking DRAINING and
// returning success, which would look identical to a drain that worked.
func TestDrainEngineWithoutDrainerIsUnimplemented(t *testing.T) {
	r := New(newMemStore())
	registeredEngine(t, r, "tp4", "my-llama")
	a := NewConnectAdapter(r)

	_, err := a.DrainEngine(context.Background(), connect.NewRequest(
		&provisionerv1.DrainEngineRequest{EngineId: "tp4", Force: true}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Errorf("code = %v, want Unimplemented", connect.CodeOf(err))
	}
	got, _ := r.Get("tp4")
	if got.GetState() == provisionerv1.EngineState_ENGINE_STATE_DRAINING {
		t.Error("member was marked DRAINING despite no drain being possible")
	}
}
