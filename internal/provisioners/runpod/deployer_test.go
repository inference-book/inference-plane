package runpod

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

func okDep() *provisionerv1.Deployment {
	return &provisionerv1.Deployment{
		Id:         "my-llama",
		InstanceId: "my-pod",
		Image:      "vllm/vllm-openai:v0.7.0",
		Model:      "Qwen/Qwen2.5-1.5B-Instruct",
		EnginePort: 8000,
		EngineArgs: []string{"--gpu-memory-utilization", "0.9"},
		Env: map[string]string{
			"HF_HUB_DISABLE_TELEMETRY": "1",
		},
	}
}

func okInst() *provisionerv1.Instance {
	return &provisionerv1.Instance{
		Id:         "my-pod",
		Provider:   "runpod",
		ProviderId: "rp-base",
		Gpu: &provisionerv1.GpuInfo{
			Sku:    "NVIDIA RTX A5000",
			Class:  "small",
			Count:  1,
			VramGb: 24,
		},
	}
}

// engineHealthServer is a localhost httptest server that serves
// /health with a configurable response. Used by the deployer tests
// to back the "publicIp:port" the fake RunPod GET response advertises.
func engineHealthServer(t *testing.T, status int) (host string, port int32) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	// Parse the listener address into host + port.
	addr := strings.TrimPrefix(srv.URL, "http://")
	parts := strings.Split(addr, ":")
	host = parts[0]
	var p int32
	fmt.Sscanf(parts[1], "%d", &p)
	port = p
	return host, port
}

func TestDeploy_HappyPath_GoesToRUNNING(t *testing.T) {
	// Stand up a real engine listener so /health probes succeed.
	host, port := engineHealthServer(t, http.StatusOK)

	var getCalls atomic.Int32
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		switch {
		case method == "POST" && path == "/pods":
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			if req.ImageName != "vllm/vllm-openai:v0.7.0" {
				t.Errorf("POST body imageName = %q, want vllm/vllm-openai:v0.7.0", req.ImageName)
			}
			joined := strings.Join(req.DockerStartCmd, " ")
			if !strings.Contains(joined, "--model Qwen/Qwen2.5-1.5B-Instruct") {
				t.Errorf("dockerStartCmd missing --model: %v", req.DockerStartCmd)
			}
			if !strings.Contains(joined, "--gpu-memory-utilization") {
				t.Errorf("dockerStartCmd missing operator engine-args: %v", req.DockerStartCmd)
			}
			if req.Env["HF_HUB_DISABLE_TELEMETRY"] != "1" {
				t.Errorf("env not propagated: %+v", req.Env)
			}
			return 201, `{"id":"rp-engine-1","desiredStatus":"RUNNING"}`
		case method == "GET" && path == "/pods/rp-engine-1":
			n := getCalls.Add(1)
			if n == 1 {
				// First GET: no publicIp yet.
				return 200, `{"id":"rp-engine-1","machine":{}}`
			}
			// Subsequent GETs: publicIp + port mapping pointing at the
			// local engine server, so the health probe hits a real 200.
			return 200, fmt.Sprintf(
				`{"id":"rp-engine-1","publicIp":%q,"portMappings":{"8000":%d},"machine":{"gpuTypeId":"NVIDIA RTX A5000"}}`,
				host, port)
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client, WithSSHReadyWait(2*time.Second, 5*time.Millisecond))

	c := &collector{}
	if err := p.Deploy(context.Background(), okDep(), okInst(), nil, c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", c.lastState())
	}
	if c.lastEndpoint() == "" {
		t.Error("expected engine endpoint in final update")
	}
	if c.lastContainer() != "rp-engine-1" {
		t.Errorf("container id = %q, want rp-engine-1", c.lastContainer())
	}
}

func TestDeploy_PostFailure_GoesToFAILED(t *testing.T) {
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/pods" {
			return 500, `{"error":"no capacity"}`
		}
		t.Errorf("unexpected %s %s after POST failure", method, path)
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client)

	c := &collector{}
	err := p.Deploy(context.Background(), okDep(), okInst(), nil, c.emit)
	if err == nil {
		t.Fatal("expected error when POST /pods fails")
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("final state = %v, want FAILED", c.lastState())
	}
}

func TestDeploy_NoSKU_OnInstance_GoesToFAILED(t *testing.T) {
	// Instance has no resolved GPU SKU -- the deployer should refuse
	// before issuing any HTTP call.
	called := atomic.Int32{}
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		called.Add(1)
		return 200, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client)

	inst := okInst()
	inst.Gpu = nil // no resolved GPU

	c := &collector{}
	if err := p.Deploy(context.Background(), okDep(), inst, nil, c.emit); err == nil {
		t.Fatal("expected error when instance has no GPU SKU")
	}
	if got := called.Load(); got != 0 {
		t.Errorf("expected zero HTTP calls when validation fails; got %d", got)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("final state = %v, want FAILED", c.lastState())
	}
}

func TestDeploy_SKUFromRequirements_AutoProvisioned(t *testing.T) {
	// Auto-provisioned instance: a PENDING shell with no resolved GPU,
	// only Spec.Requirements. The deployer resolves the SKU from the
	// requirements (here an explicit Sku) and stamps it into the POST.
	host, port := engineHealthServer(t, http.StatusOK)

	var sawSKU string
	var getCalls atomic.Int32
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		switch {
		case method == "POST" && path == "/pods":
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			if len(req.GPUTypeIDs) > 0 {
				sawSKU = req.GPUTypeIDs[0]
			}
			if req.GPUCount != 2 {
				t.Errorf("GPUCount = %d, want 2 (from requirements)", req.GPUCount)
			}
			return 201, `{"id":"rp-auto-1"}`
		case method == "GET" && path == "/pods/rp-auto-1":
			if getCalls.Add(1) == 1 {
				return 200, `{"id":"rp-auto-1","machine":{}}`
			}
			return 200, fmt.Sprintf(
				`{"id":"rp-auto-1","publicIp":%q,"portMappings":{"8000":%d}}`, host, port)
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client, WithSSHReadyWait(2*time.Second, 5*time.Millisecond))

	inst := okInst()
	inst.Gpu = nil // PENDING shell, GPU not yet resolved
	inst.Spec = &provisionerv1.Spec{
		Requirements: &provisionerv1.ResourceRequirements{
			Sku:      "NVIDIA RTX A5000",
			GpuCount: 2,
		},
	}

	c := &collector{}
	if err := p.Deploy(context.Background(), okDep(), inst, nil, c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if sawSKU != "NVIDIA RTX A5000" {
		t.Errorf("POST GPUTypeIDs[0] = %q, want SKU resolved from requirements", sawSKU)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		t.Errorf("final state = %v, want RUNNING", c.lastState())
	}
}

func TestDeploy_HealthTimeout_GoesToFAILED(t *testing.T) {
	// /health never returns 2xx -- engine listener serves 503.
	host, port := engineHealthServer(t, http.StatusServiceUnavailable)

	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		switch {
		case method == "POST" && path == "/pods":
			return 201, `{"id":"rp-engine-2"}`
		case method == "GET" && path == "/pods/rp-engine-2":
			return 200, fmt.Sprintf(
				`{"id":"rp-engine-2","publicIp":%q,"portMappings":{"8000":%d}}`,
				host, port)
		}
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	// Tight timeout so the test finishes fast.
	p := New(client, WithSSHReadyWait(30*time.Millisecond, 5*time.Millisecond))

	c := &collector{}
	err := p.Deploy(context.Background(), okDep(), okInst(), nil, c.emit)
	if err == nil {
		t.Fatal("expected error when /health never returns 2xx")
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Errorf("final state = %v, want FAILED", c.lastState())
	}
}

func TestDestroy_HappyPath(t *testing.T) {
	var deleteCalls atomic.Int32
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "DELETE" && path == "/pods/rp-engine-9" {
			deleteCalls.Add(1)
			return 204, ""
		}
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client)

	dep := okDep()
	dep.ContainerId = "rp-engine-9"

	c := &collector{}
	if err := p.Destroy(context.Background(), dep, okInst(), nil, c.emit); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("final state = %v, want TERMINATED", c.lastState())
	}
	if got := deleteCalls.Load(); got != 1 {
		t.Errorf("DELETE calls = %d, want 1", got)
	}
}

func TestDestroy_AlreadyGone_StillTERMINATED(t *testing.T) {
	// No container id on record -- treat as "nothing to do, already
	// terminal." Mirrors sshdocker's idempotent destroy.
	called := atomic.Int32{}
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		called.Add(1)
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client)

	dep := okDep()
	dep.ContainerId = ""

	c := &collector{}
	if err := p.Destroy(context.Background(), dep, okInst(), nil, c.emit); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("final state = %v, want TERMINATED", c.lastState())
	}
	if got := called.Load(); got != 0 {
		t.Errorf("DELETE should not fire when no container id; got %d calls", got)
	}
}

// collector mirrors the sshdocker test helper -- captures every emit
// so tests can assert on the sequence.
type collector struct {
	updates []provisioners.DeployStateUpdate
}

func (c *collector) emit(u provisioners.DeployStateUpdate) { c.updates = append(c.updates, u) }

func (c *collector) lastState() provisionerv1.DeploymentState {
	if len(c.updates) == 0 {
		return provisionerv1.DeploymentState_DEPLOYMENT_STATE_UNSPECIFIED
	}
	return c.updates[len(c.updates)-1].State
}

func (c *collector) lastEndpoint() string {
	if len(c.updates) == 0 {
		return ""
	}
	return c.updates[len(c.updates)-1].EngineEndpoint
}

func (c *collector) lastContainer() string {
	if len(c.updates) == 0 {
		return ""
	}
	return c.updates[len(c.updates)-1].ContainerID
}
