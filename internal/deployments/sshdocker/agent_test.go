package sshdocker

import (
	"strings"
	"testing"

	"github.com/inference-book/inference-plane/internal/engineagent"
	"github.com/inference-book/inference-plane/internal/version"
)

func releaseBuild(t *testing.T) {
	t.Helper()
	orig := version.Version
	version.Version = "v0.2.3"
	t.Cleanup(func() { version.Version = orig })
	t.Setenv(engineagent.EnvBinaryURL, "")
}

func agentSpec() AgentSpec {
	return AgentSpec{
		DeploymentID: "dep-1",
		Image:        "vllm/vllm-openai:v0.7.0",
		Env: map[string]string{
			engineagent.EnvEngineID:   "dep-1",
			engineagent.EnvServiceURL: "https://cp.example.com",
			engineagent.EnvHealthURL:  "http://127.0.0.1:8000/health",
		},
	}
}

func TestRunAgentCommandShape(t *testing.T) {
	releaseBuild(t)
	r := &fakeRunner{}
	d := NewDocker(r)

	if _, err := d.RunAgent(t.Context(), agentSpec()); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	cmd := strings.Join(r.calls, "\n")

	// The whole point of this path: the agent shares the engine's network
	// namespace, so its health probe reaches the engine on 127.0.0.1 and
	// ASSEMBLING stays observable before anything is externally reachable.
	if !strings.Contains(cmd, "--network 'container:iplane-deployment-dep-1'") {
		t.Errorf("agent does not join the engine's netns:\n%s", cmd)
	}
	// The engine image is reused, so the sidecar costs no extra pull on a
	// box that just spent minutes fetching a multi-GB image.
	if !strings.Contains(cmd, "'vllm/vllm-openai:v0.7.0'") {
		t.Errorf("agent does not reuse the engine image:\n%s", cmd)
	}
	// --entrypoint is what makes engine_entrypoint unnecessary here: we are
	// not wrapping the engine, so we never need to know what it runs.
	if !strings.Contains(cmd, "--entrypoint /bin/sh") {
		t.Errorf("agent does not override the image entrypoint:\n%s", cmd)
	}
	if !strings.Contains(cmd, "--name 'iplane-agent-dep-1'") {
		t.Errorf("agent container is not distinctly named:\n%s", cmd)
	}
	// Distinguishable from the engine in `docker ps`.
	if !strings.Contains(cmd, "iplane.role=agent") {
		t.Errorf("agent container is not labelled as such:\n%s", cmd)
	}
}

func TestRunAgentPassesInjectedIdentity(t *testing.T) {
	releaseBuild(t)
	r := &fakeRunner{}
	d := NewDocker(r)

	if _, err := d.RunAgent(t.Context(), agentSpec()); err != nil {
		t.Fatal(err)
	}

	cmd := strings.Join(r.calls, "\n")
	for _, want := range []string{
		"IPLANE_ENGINE_ID=dep-1",
		"IPLANE_SERVICE_URL=https://cp.example.com",
		"IPLANE_ENGINE_HEALTH_URL=http://127.0.0.1:8000/health",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("agent env missing %q:\n%s", want, cmd)
		}
	}
}

// The agent IS this container's job, so a failed fetch should exit non-zero
// and let docker's restart policy retry, rather than leaving a container
// alive doing nothing. That inverts the wrapper's swallow-everything rule,
// and the reason is that failing here costs no inference.
func TestAgentScriptFailsLoudly(t *testing.T) {
	got, err := engineagent.AgentScript("https://example.com/iplane")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "set -e") {
		t.Errorf("agent script does not fail on a bad fetch:\n%s", got)
	}
	if strings.Contains(got, "|| true") {
		t.Errorf("agent script swallows failure; that is the wrapper's rule, not this one:\n%s", got)
	}
	if !strings.Contains(got, "exec /tmp/iplane engine-agent") {
		t.Errorf("agent script does not exec the agent:\n%s", got)
	}
}

func TestRunAgentRefusesWithoutIdentity(t *testing.T) {
	releaseBuild(t)
	d := NewDocker(&fakeRunner{})

	spec := agentSpec()
	spec.Env = map[string]string{}

	if _, err := d.RunAgent(t.Context(), spec); err == nil {
		t.Error("want an error with no identity stamped")
	}
}

// A dev build has no published artifact, so there is nothing to fetch. The
// deploy still succeeds; the caller swallows this.
func TestRunAgentRefusesOnDevBuild(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	t.Cleanup(func() { version.Version = orig })
	t.Setenv(engineagent.EnvBinaryURL, "")

	d := NewDocker(&fakeRunner{})

	if _, err := d.RunAgent(t.Context(), agentSpec()); err == nil {
		t.Error("want an error when no agent binary is published")
	}
}

func TestStopAgentTargetsOnlyTheSidecar(t *testing.T) {
	r := &fakeRunner{}
	d := NewDocker(r)

	if err := d.StopAgent(t.Context(), "dep-1"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}

	all := strings.Join(r.calls, "\n")
	if !strings.Contains(all, "iplane-agent-dep-1") {
		t.Errorf("StopAgent did not target the sidecar:\n%s", all)
	}
	// The engine container must survive an agent teardown.
	if strings.Contains(all, "iplane-deployment-dep-1") {
		t.Errorf("StopAgent touched the engine container:\n%s", all)
	}
}

func TestAgentContainerNameIsDistinctFromTheEngine(t *testing.T) {
	if AgentContainerName("dep-1") == ContainerName("dep-1") {
		t.Error("agent and engine containers share a name; docker would refuse the second")
	}
}
