package provisioners

import (
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/engineagent"
)

func baseDep() *provisionerv1.Deployment {
	return &provisionerv1.Deployment{
		Id:         "dep-1",
		Model:      "meta-llama/Llama-3.1-8B",
		EnginePort: 8000,
		Env:        map[string]string{"HF_TOKEN": "hf_secret"},
	}
}

func TestWithAgentEnvStampsIdentity(t *testing.T) {
	got := withAgentEnv(baseDep(), "dep-1-r2", 2, "runpod", "https://cp.example.com")

	for k, want := range map[string]string{
		engineagent.EnvEngineID:     "dep-1-r2",
		engineagent.EnvDeploymentID: "dep-1",
		engineagent.EnvModel:        "meta-llama/Llama-3.1-8B",
		engineagent.EnvProvider:     "runpod",
		engineagent.EnvNodeIndex:    "2",
		engineagent.EnvServiceURL:   "https://cp.example.com",
		engineagent.EnvHealthURL:    "http://127.0.0.1:8000/health",
	} {
		if got.GetEnv()[k] != want {
			t.Errorf("env[%s] = %q, want %q", k, got.GetEnv()[k], want)
		}
	}
	// The operator's own env has to survive alongside ours.
	if got.GetEnv()["HF_TOKEN"] != "hf_secret" {
		t.Error("operator-supplied HF_TOKEN was dropped")
	}
}

// Every slot in a fan-out reads the same record, so stamping must not mutate
// it. This was the shape of the lost-update bug in stores/file, and the same
// hazard applies to a shared proto.
func TestWithAgentEnvDoesNotMutateTheSharedRecord(t *testing.T) {
	dep := baseDep()
	_ = withAgentEnv(dep, "dep-1-r0", 0, "runpod", "https://cp.example.com")

	if _, leaked := dep.GetEnv()[engineagent.EnvEngineID]; leaked {
		t.Error("stamping wrote through to the shared deployment record")
	}
	if len(dep.GetEnv()) != 1 {
		t.Errorf("shared record env grew to %d keys", len(dep.GetEnv()))
	}
}

// Deployment.Env is the operator's pass-through. Silently overwriting a key
// they set would be the kind of surprise that costs an afternoon.
func TestOperatorSuppliedValuesWin(t *testing.T) {
	dep := baseDep()
	dep.Env[engineagent.EnvServiceURL] = "https://operator-tunnel.example.com"
	dep.Env[engineagent.EnvHealthURL] = "http://127.0.0.1:9999/healthz"

	got := withAgentEnv(dep, "dep-1-r0", 0, "runpod", "https://cp.example.com")

	if got.GetEnv()[engineagent.EnvServiceURL] != "https://operator-tunnel.example.com" {
		t.Errorf("service url = %q, want the operator's value", got.GetEnv()[engineagent.EnvServiceURL])
	}
	if got.GetEnv()[engineagent.EnvHealthURL] != "http://127.0.0.1:9999/healthz" {
		t.Errorf("health url = %q, want the operator's value", got.GetEnv()[engineagent.EnvHealthURL])
	}
}

// An unstamped service URL means the agent does not register, which shows up
// as a missing member rather than one pointed at a wrong address.
func TestNoServiceURLLeavesTheKeyUnset(t *testing.T) {
	got := withAgentEnv(baseDep(), "dep-1-r0", 0, "runpod", "")

	if _, set := got.GetEnv()[engineagent.EnvServiceURL]; set {
		t.Error("service url stamped despite the daemon not knowing its own address")
	}
	// The rest of the identity is still stamped: it costs nothing and makes
	// a hand-run agent on the box work with only --service-url.
	if got.GetEnv()[engineagent.EnvEngineID] != "dep-1-r0" {
		t.Error("engine id should still be stamped without a service url")
	}
}

func TestNilEnvIsCreated(t *testing.T) {
	dep := &provisionerv1.Deployment{Id: "dep-1", Model: "m", EnginePort: 8000}

	got := withAgentEnv(dep, "dep-1", 0, "vast", "https://cp.example.com")

	if got.GetEnv()[engineagent.EnvEngineID] != "dep-1" {
		t.Errorf("env not created for a deployment with no operator env: %v", got.GetEnv())
	}
}

// Slot 0 is a real value, not an absent one. An off-by-one here would
// attribute every replica to the wrong node.
func TestSlotZeroIsStamped(t *testing.T) {
	got := withAgentEnv(baseDep(), "dep-1", 0, "runpod", "https://cp.example.com")

	if got.GetEnv()[engineagent.EnvNodeIndex] != "0" {
		t.Errorf("node index = %q, want \"0\"", got.GetEnv()[engineagent.EnvNodeIndex])
	}
}

func TestUnsetEnginePortLeavesHealthURLUnset(t *testing.T) {
	dep := baseDep()
	dep.EnginePort = 0

	got := withAgentEnv(dep, "dep-1", 0, "runpod", "https://cp.example.com")

	if _, set := got.GetEnv()[engineagent.EnvHealthURL]; set {
		t.Error("health url stamped from a zero engine port; the agent's own default is better than a wrong port")
	}
}
