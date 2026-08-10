package runpod

import (
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/engineagent"
	"github.com/inference-book/inference-plane/internal/version"
)

func agentEnv() map[string]string {
	return map[string]string{
		engineagent.EnvEngineID:   "dep-1-r0",
		engineagent.EnvServiceURL: "https://cp.example.com",
	}
}

func wrappedDep() *provisionerv1.Deployment {
	return &provisionerv1.Deployment{
		Id:               "dep-1",
		Model:            "meta-llama/Llama-3.1-8B",
		EngineEntrypoint: []string{"vllm", "serve"},
	}
}

func releaseBuild(t *testing.T) {
	t.Helper()
	orig := version.Version
	version.Version = "v0.2.3"
	t.Cleanup(func() { version.Version = orig })
	t.Setenv(engineagent.EnvBinaryURL, "")
}

func TestAgentEntrypointWrapsWhenFullyConfigured(t *testing.T) {
	releaseBuild(t)

	got := agentEntrypoint(wrappedDep(), agentEnv())

	if len(got) != 4 {
		t.Fatalf("entrypoint = %v, want 4 elements", got)
	}
	if got[0] != "/bin/sh" || got[1] != "-c" {
		t.Errorf("entrypoint does not start with a shell: %v", got[:2])
	}
	if !strings.Contains(got[2], `exec 'vllm' 'serve' "$@"`) {
		t.Errorf("script does not exec the engine:\n%s", got[2])
	}
	// Docker runs ENTRYPOINT + CMD as one argv, so without this filler the
	// first engine arg would bind to $0 and vanish from "$@".
	if got[3] != "iplane-engine-agent" {
		t.Errorf("argv[3] = %q, want the $0 placeholder", got[3])
	}
}

// nil is the safe answer and the common one. Each precondition is checked
// independently, because any of them missing means the deployment should go
// out exactly as it did before the agent existed.
func TestAgentEntrypointIsNilWithoutItsPreconditions(t *testing.T) {
	t.Run("no engine entrypoint", func(t *testing.T) {
		releaseBuild(t)
		dep := wrappedDep()
		dep.EngineEntrypoint = nil

		if got := agentEntrypoint(dep, agentEnv()); got != nil {
			t.Errorf("entrypoint = %v, want nil with no engine command to exec", got)
		}
	})

	t.Run("no agent identity stamped", func(t *testing.T) {
		releaseBuild(t)

		if got := agentEntrypoint(wrappedDep(), map[string]string{}); got != nil {
			t.Errorf("entrypoint = %v, want nil with nothing to register as", got)
		}
	})

	t.Run("no service url", func(t *testing.T) {
		releaseBuild(t)
		env := agentEnv()
		delete(env, engineagent.EnvServiceURL)

		if got := agentEntrypoint(wrappedDep(), env); got != nil {
			t.Errorf("entrypoint = %v, want nil with nowhere to register", got)
		}
	})

	t.Run("dev build has no published agent", func(t *testing.T) {
		orig := version.Version
		version.Version = "dev"
		t.Cleanup(func() { version.Version = orig })
		t.Setenv(engineagent.EnvBinaryURL, "")

		if got := agentEntrypoint(wrappedDep(), agentEnv()); got != nil {
			t.Errorf("entrypoint = %v, want nil with no artifact to fetch", got)
		}
	})
}

// The pod request is where the decision becomes visible, so assert it there
// too: an unconfigured deployment must produce the same request it always
// did, with the image's own entrypoint left alone.
func TestBuildEnginePodRequestLeavesEntrypointAloneByDefault(t *testing.T) {
	releaseBuild(t)
	dep := &provisionerv1.Deployment{
		Id:         "dep-1",
		Image:      "vllm/vllm-openai:v0.7.0",
		Model:      "meta-llama/Llama-3.1-8B",
		EnginePort: 8000,
	}
	inst := &provisionerv1.Instance{
		Id:       "dep-1",
		Provider: "runpod",
		Hardware: &provisionerv1.Hardware{GpuSku: "NVIDIA A100 80GB PCIe", GpuCount: 1},
	}

	req, err := buildEnginePodRequest(dep, inst)
	if err != nil {
		t.Fatal(err)
	}
	if req.DockerEntrypoint != nil {
		t.Errorf("DockerEntrypoint = %v, want nil for a deployment with no agent configured", req.DockerEntrypoint)
	}
	// The engine args are untouched either way.
	if !strings.Contains(strings.Join(req.DockerStartCmd, " "), "--model meta-llama/Llama-3.1-8B") {
		t.Errorf("DockerStartCmd = %v", req.DockerStartCmd)
	}
}
