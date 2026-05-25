package cmd

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners/state"
)

// fakeEngine stands in for vLLM's OpenAI-compat surface. Returns a
// canned chat-completion response and records the last request body
// so tests can assert on what the CLI sent.
type fakeEngine struct {
	statusCode int
	respBody   string
	lastReq    []byte
}

func (f *fakeEngine) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		f.lastReq = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.statusCode)
		_, _ = io.WriteString(w, f.respBody)
	})
}

// seedRunningDeployment writes a RUNNING deployment to the test env's
// state file, pointing engine_endpoint at the fakeEngine. Mirrors the
// shape finalizeInstanceAfterDeploy + patchDeployment land for a
// healthy deploy.
func seedRunningDeployment(t *testing.T, store *state.Store, id, modelID, endpoint string) {
	t.Helper()
	if err := store.Update(func(f *state.File) error {
		f.Deployments[id] = &provisionerv1.Deployment{
			Id:             id,
			InstanceId:     "my-pod",
			Image:          "vllm/vllm-openai:test",
			Model:          modelID,
			EnginePort:     8000,
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: endpoint,
			ContainerId:    "fake-container",
		}
		return nil
	}); err != nil {
		t.Fatalf("seed deployment: %v", err)
	}
}

func TestQuery_HappyPath_PrintsResponseAndTiming(t *testing.T) {
	engine := &fakeEngine{
		statusCode: 200,
		respBody: `{
			"choices": [{
				"message": {"role": "assistant", "content": "Paris."},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 8, "completion_tokens": 1, "total_tokens": 9}
		}`,
	}
	engSrv := httptest.NewServer(engine.handler())
	t.Cleanup(engSrv.Close)

	env := newDeploymentTestEnv(t)
	seedRunningDeployment(t, env.store, "my-llama", "Qwen/Qwen2.5-1.5B-Instruct", engSrv.URL)

	out, err := runDeploymentCmd(t, env,
		"query", "my-llama", "What is the capital of France?",
	)
	if err != nil {
		t.Fatalf("query: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Paris.") {
		t.Errorf("response text missing from output: %s", out)
	}
	if !strings.Contains(out, "prompt 8 tok") {
		t.Errorf("token summary missing: %s", out)
	}

	// Assert what the CLI sent to the engine.
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role, Content string
		} `json:"messages"`
	}
	if err := json.Unmarshal(engine.lastReq, &req); err != nil {
		t.Fatalf("decode sent request: %v", err)
	}
	if req.Model != "Qwen/Qwen2.5-1.5B-Instruct" {
		t.Errorf("sent model = %q, want Qwen/Qwen2.5-1.5B-Instruct", req.Model)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Errorf("sent messages = %+v, want one user message", req.Messages)
	}
	if req.Messages[0].Content != "What is the capital of France?" {
		t.Errorf("sent prompt = %q, want the operator's prompt", req.Messages[0].Content)
	}
}

func TestQuery_WithSystemPrompt_PrependsSystemMessage(t *testing.T) {
	engine := &fakeEngine{
		statusCode: 200,
		respBody: `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}
	engSrv := httptest.NewServer(engine.handler())
	t.Cleanup(engSrv.Close)

	env := newDeploymentTestEnv(t)
	seedRunningDeployment(t, env.store, "my-llama", "Qwen/Qwen2.5-1.5B-Instruct", engSrv.URL)

	if _, err := runDeploymentCmd(t, env,
		"query", "my-llama", "hi",
		"--system", "Be concise.",
	); err != nil {
		t.Fatalf("query: %v", err)
	}

	var req struct {
		Messages []struct {
			Role, Content string
		} `json:"messages"`
	}
	if err := json.Unmarshal(engine.lastReq, &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("want 2 messages (system+user); got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "Be concise." {
		t.Errorf("system message = %+v, want first slot with operator-supplied content", req.Messages[0])
	}
}

func TestQuery_DeploymentNotRunning_Refuses(t *testing.T) {
	env := newDeploymentTestEnv(t)
	// Seed a PENDING deployment -- query must refuse before dialing.
	_ = env.store.Update(func(f *state.File) error {
		f.Deployments["my-llama"] = &provisionerv1.Deployment{
			Id: "my-llama", InstanceId: "my-pod",
			Model: "Qwen/Qwen2.5-1.5B-Instruct",
			State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
		}
		return nil
	})

	_, err := runDeploymentCmd(t, env, "query", "my-llama", "hi")
	if err == nil {
		t.Fatal("expected error for PENDING deployment; got nil")
	}
	if !strings.Contains(err.Error(), "PENDING") {
		t.Errorf("error should mention PENDING state; got: %v", err)
	}
}

func TestQuery_EngineReturns500_SurfacesEngineError(t *testing.T) {
	engine := &fakeEngine{
		statusCode: 500,
		respBody:   `{"error":"model not loaded"}`,
	}
	engSrv := httptest.NewServer(engine.handler())
	t.Cleanup(engSrv.Close)

	env := newDeploymentTestEnv(t)
	seedRunningDeployment(t, env.store, "my-llama", "Qwen/Qwen2.5-1.5B-Instruct", engSrv.URL)

	_, err := runDeploymentCmd(t, env, "query", "my-llama", "hi")
	if err == nil {
		t.Fatal("expected error on 500; got nil")
	}
	if !strings.Contains(err.Error(), "model not loaded") {
		t.Errorf("error should surface engine response body; got: %v", err)
	}
}

func TestQuery_JSONOutput_PassesBodyThrough(t *testing.T) {
	engine := &fakeEngine{
		statusCode: 200,
		respBody:   `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}
	engSrv := httptest.NewServer(engine.handler())
	t.Cleanup(engSrv.Close)

	env := newDeploymentTestEnv(t)
	seedRunningDeployment(t, env.store, "my-llama", "Qwen/Qwen2.5-1.5B-Instruct", engSrv.URL)

	out, err := runDeploymentCmd(t, env,
		"query", "my-llama", "hi",
		"--output", "json",
	)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	// JSON path emits the engine response (pretty-printed); the id
	// field is the cheapest stable signal we sent it untouched.
	if !strings.Contains(out, `"id": "chatcmpl-1"`) {
		t.Errorf("JSON output should pretty-print the engine response; got: %s", out)
	}
}

func TestDeploy_NoWait_InProcess_Refuses(t *testing.T) {
	// In-process mode (no --service-url) + --no-wait would kill the
	// executor goroutine when the CLI exits. The CLI must refuse with
	// a clear pointer at --service-url. Issue #35.
	resetDeploymentFlags()
	rootCmd.SetArgs([]string{
		"deployment", "deploy", "my-llama",
		"--instance", "my-pod",
		"--image", "vllm/vllm-openai:0.7.0",
		"--model", "Qwen/Qwen2.5-1.5B-Instruct",
		"--wait=false",
		// Deliberately NO --service-url.
	})
	rootCmd.SetOut(io.Discard)
	rootCmd.SetErr(io.Discard)
	err := rootCmd.Execute()
	if err == nil {
		t.Fatal("expected error for --no-wait without --service-url; got nil")
	}
	if !strings.Contains(err.Error(), "--service-url") {
		t.Errorf("error should point at --service-url; got: %v", err)
	}
}
