package vast

import (
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/engineagent"
)

func depWithAgentEnv(t *testing.T) *provisionerv1.Deployment {
	t.Helper()
	t.Setenv(engineagent.EnvBinaryURL, "https://example.invalid/iplane-linux-amd64")
	return &provisionerv1.Deployment{
		Id:    "d1",
		Model: "Qwen/Qwen2.5-72B-Instruct",
		Env: map[string]string{
			engineagent.EnvEngineID:   "d1-0",
			engineagent.EnvServiceURL: "http://cp.invalid:8080",
		},
	}
}

// The agent has to actually be launched, and the engine has to remain the
// container's main process afterwards.
func TestOnstartLaunchesTheAgentAndStillExecsTheEngine(t *testing.T) {
	dep := depWithAgentEnv(t)
	script := onstartScript([]string{"python3", "-m", "vllm.entrypoints.openai.api_server"}, dep, 8000)

	if !strings.Contains(script, "engine-agent") {
		t.Fatalf("agent never started:\n%s", script)
	}
	// exec, not spawn: the container's lifetime must still track the engine.
	if !strings.Contains(script, "exec '") {
		t.Errorf("engine is not exec'd, so the container no longer tracks it:\n%s", script)
	}
	// The agent must be backgrounded before the exec, or it never runs: exec
	// replaces the shell and nothing after it is reached.
	if strings.Index(script, "engine-agent") > strings.Index(script, "exec '") {
		t.Errorf("agent is started after the exec and can never run:\n%s", script)
	}
}

// No identity means nothing to register as; no published binary means nothing
// to fetch. Either way the script must be byte-identical to what shipped
// before the agent existed, because a deploy is not allowed to change shape
// on an untagged build.
func TestOnstartUnchangedWithoutIdentityOrBinary(t *testing.T) {
	engineCmd := []string{"python3", "-m", "vllm.entrypoints.openai.api_server"}
	bare := &provisionerv1.Deployment{Id: "d1", Model: "m"}

	t.Run("no identity stamped", func(t *testing.T) {
		t.Setenv(engineagent.EnvBinaryURL, "https://example.invalid/iplane-linux-amd64")
		got := onstartScript(engineCmd, bare, 8000)
		if strings.Contains(got, "engine-agent") {
			t.Errorf("started an agent with no identity to register as:\n%s", got)
		}
		if !strings.HasPrefix(got, "exec ") {
			t.Errorf("script changed shape without an agent:\n%s", got)
		}
	})

	// A dev build publishes no artifact, which is deliberate: a pod pulling an
	// agent from a different version than the control plane it registers with
	// is a bug nobody wants to debug from a rented box. Built inline rather
	// than via the helper, which sets a URL.
	t.Run("no binary published", func(t *testing.T) {
		t.Setenv(engineagent.EnvBinaryURL, "")
		identified := &provisionerv1.Deployment{
			Id: "d1", Model: "m",
			Env: map[string]string{
				engineagent.EnvEngineID:   "d1-0",
				engineagent.EnvServiceURL: "http://cp.invalid:8080",
			},
		}
		got := onstartScript(engineCmd, identified, 8000)
		if strings.Contains(got, "engine-agent") {
			t.Errorf("fetched an agent with no published artifact:\n%s", got)
		}
		if !strings.HasPrefix(got, "exec ") {
			t.Errorf("script changed shape without an agent:\n%s", got)
		}
	})
}

// The engine argv is operator-supplied and becomes a script on a machine we
// are paying for. Adding the prelude must not disturb the quoting.
func TestOnstartStillQuotesHostileArgv(t *testing.T) {
	dep := depWithAgentEnv(t)
	dep.EngineArgs = []string{"--served-model-name", "it's; rm -rf /"}
	script := onstartScript([]string{"python3"}, dep, 8000)

	if strings.Contains(script, "; rm -rf /") && !strings.Contains(script, `'\''`) {
		t.Errorf("hostile argument escaped its quoting:\n%s", script)
	}
	if !strings.Contains(script, `'it'\''s; rm -rf /'`) {
		t.Errorf("expected the embedded quote to be escaped:\n%s", script)
	}
}

// Both providers fetch the agent the same way. If these ever diverge it will
// be because someone edited one copy, which is the thing extracting the
// prelude was meant to prevent.
func TestVastAndRunpodShareTheSameFetch(t *testing.T) {
	const url = "https://example.invalid/iplane-linux-amd64"
	prelude, err := engineagent.AgentPrelude(url)
	if err != nil {
		t.Fatalf("AgentPrelude: %v", err)
	}
	wrapper, err := engineagent.WrapperScript(url, []string{"python3"})
	if err != nil {
		t.Fatalf("WrapperScript: %v", err)
	}
	if !strings.HasPrefix(wrapper, prelude) {
		t.Error("the entrypoint wrapper no longer starts with the shared prelude; the two fetch paths have diverged")
	}
	// The difference between them is exactly the exec line, and that is the
	// difference that justified the split.
	if strings.Contains(prelude, `"$@"`) {
		t.Error(`prelude carries "$@", which is meaningless in a standalone startup script`)
	}
}
