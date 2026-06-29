package runpod

import (
	"context"
	"encoding/json"
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
		Hardware: &provisionerv1.Hardware{
			GpuSku:    "NVIDIA RTX A5000",
			GpuCount:  1,
			GpuVramMb: 24 * 1024,
		},
	}
}

// proxyServer stands in for proxy.runpod.net during tests. The
// deployer's health probe dials <base>/<pod-id>-<port>/health (see
// waitForEngineReady's proxyBaseURL branch); this server matches that
// path shape and returns the configured status code.
func proxyServer(t *testing.T, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// e.g. /rp-engine-1-8000/health
		if strings.HasSuffix(r.URL.Path, "/health") {
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestDeploy_HappyPath_GoesToRUNNING(t *testing.T) {
	proxyURL := proxyServer(t, http.StatusOK)
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/pods" {
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
			// Default deploy is proxy-only: no publicIp, no /tcp ports
			// other than the engine /http.
			if req.SupportPublicIP {
				t.Errorf("SupportPublicIP=true on default deploy; want false (proxy-only is the cost-aware default)")
			}
			if len(req.Ports) != 1 || req.Ports[0] != "8000/http" {
				t.Errorf("Ports = %v, want [8000/http]", req.Ports)
			}
			return 201, `{"id":"rp-engine-1","desiredStatus":"RUNNING"}`
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client,
		WithSSHReadyWait(2*time.Second, 5*time.Millisecond),
		WithProxyBaseURL(proxyURL),
	)

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
	inst.Hardware = nil // no resolved GPU

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
	proxyURL := proxyServer(t, http.StatusOK)

	var sawSKU string
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/pods" {
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			if len(req.GPUTypeIDs) > 0 {
				sawSKU = req.GPUTypeIDs[0]
			}
			if req.GPUCount != 2 {
				t.Errorf("GPUCount = %d, want 2 (from requirements)", req.GPUCount)
			}
			return 201, `{"id":"rp-auto-1"}`
		}
		t.Errorf("unexpected %s %s", method, path)
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client,
		WithSSHReadyWait(2*time.Second, 5*time.Millisecond),
		WithProxyBaseURL(proxyURL),
	)

	inst := okInst()
	inst.Hardware = nil // PENDING shell, GPU not yet resolved
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

func TestDeploy_PassesFullSKUListWhenAutoProvisioned(t *testing.T) {
	// Class-only / min-vram requirements (no explicit SKU): the deployer
	// hands RunPod the FULL cheapest-first match list so the platform
	// can route around per-SKU capacity outages. Pinning matches[0]
	// would 500 on "no capacity" whenever the cheapest is saturated.
	proxyURL := proxyServer(t, http.StatusOK)

	var sawSKUs []string
	var sawPriority string
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/pods" {
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			sawSKUs = req.GPUTypeIDs
			sawPriority = req.GPUTypePriority
			return 201, `{"id":"rp-multi-1"}`
		}
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client,
		WithSSHReadyWait(2*time.Second, 5*time.Millisecond),
		WithProxyBaseURL(proxyURL),
	)

	inst := okInst()
	inst.Hardware = nil
	inst.Spec = &provisionerv1.Spec{
		Requirements: &provisionerv1.ResourceRequirements{
			MinVramGb: 24,
			GpuCount:  1,
		},
	}

	c := &collector{}
	if err := p.Deploy(context.Background(), okDep(), inst, nil, c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if len(sawSKUs) < 2 {
		t.Errorf("GPUTypeIDs len = %d, want >=2 (full match list, not pinned matches[0])", len(sawSKUs))
	}
	if sawPriority != "availability" {
		t.Errorf("gpuTypePriority = %q, want availability", sawPriority)
	}
}

func TestDeploy_DebugShell_OptsIntoPublicIPAndSSHPort(t *testing.T) {
	// debug_shell=true is the operator opt-in: pay for publicIp +
	// expose sshd on a NAT-mapped 22/tcp. Default flow is proxy-only;
	// this verifies the opt-in flips both fields on the POST body.
	proxyURL := proxyServer(t, http.StatusOK)

	var sawPorts []string
	var sawSupportIP bool
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/pods" {
			var req createPodRequest
			_ = json.Unmarshal(body, &req)
			sawPorts = req.Ports
			sawSupportIP = req.SupportPublicIP
			return 201, `{"id":"rp-debug-1"}`
		}
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client,
		WithSSHReadyWait(2*time.Second, 5*time.Millisecond),
		WithProxyBaseURL(proxyURL),
	)

	dep := okDep()
	dep.DebugShell = true

	c := &collector{}
	if err := p.Deploy(context.Background(), dep, okInst(), nil, c.emit); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if !sawSupportIP {
		t.Error("SupportPublicIP=false on debug_shell=true; want true")
	}
	wantPorts := map[string]bool{"8000/http": true, "22/tcp": true}
	got := map[string]bool{}
	for _, p := range sawPorts {
		got[p] = true
	}
	for p := range wantPorts {
		if !got[p] {
			t.Errorf("Ports missing %q; got %v", p, sawPorts)
		}
	}
}

func TestDeploy_HealthTimeout_GoesToFAILED(t *testing.T) {
	// Proxy never returns 2xx -- stays at 503.
	proxyURL := proxyServer(t, http.StatusServiceUnavailable)

	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/pods" {
			return 201, `{"id":"rp-engine-2"}`
		}
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	// Tight timeout so the test finishes fast.
	p := New(client,
		WithSSHReadyWait(30*time.Millisecond, 5*time.Millisecond),
		WithProxyBaseURL(proxyURL),
	)

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
	// No pod id anywhere on record -- treat as "nothing to do, already
	// terminal." Mirrors sshdocker's idempotent destroy. Both
	// dep.container_id (v0.1 1:1 shape) AND inst.provider_id (v0.2
	// auto-provision shape) must be empty for this branch to fire.
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
	inst := okInst()
	inst.ProviderId = ""

	c := &collector{}
	if err := p.Destroy(context.Background(), dep, inst, nil, c.emit); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("final state = %v, want TERMINATED", c.lastState())
	}
	if got := called.Load(); got != 0 {
		t.Errorf("DELETE should not fire when no pod id; got %d calls", got)
	}
}

// TestDestroy_AutoProvisionedReadsInstanceProviderID is the regression
// gate for the leaked-pod bug. v0.2 auto-provisioned deployments stamp
// the RunPod pod id onto Instance.provider_id (not Deployment.container_id
// -- that's reserved for the v0.1 1:1 singular shape). When the deployer
// ignored the inst parameter and read only dep.GetContainerId(), Destroy
// silently no-op'd and the pod stayed alive on RunPod's side. This test
// pins the inst.provider_id fallback so the regression cannot recur.
func TestDestroy_AutoProvisionedReadsInstanceProviderID(t *testing.T) {
	var deletedPath atomic.Value
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "DELETE" {
			deletedPath.Store(path)
			return 204, ""
		}
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client)

	dep := okDep()
	dep.ContainerId = "" // v0.2 auto-provision: container_id NOT on the Deployment
	inst := okInst()
	inst.ProviderId = "mw0gmuyupiujzr" // ... it's on the Instance

	c := &collector{}
	if err := p.Destroy(context.Background(), dep, inst, nil, c.emit); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("final state = %v, want TERMINATED", c.lastState())
	}
	got, _ := deletedPath.Load().(string)
	if got != "/pods/mw0gmuyupiujzr" {
		t.Errorf("DELETE path = %q, want /pods/mw0gmuyupiujzr (regression: deployer ignored inst.provider_id)", got)
	}
}

// TestDestroy_TransientError_StaysTerminating pins the issue-165
// behavior: a 5xx (or network-error) from RunPod during DELETE should
// NOT mark the deployment FAILED. It leaves the deployment in
// TERMINATING so the reaper's TERMINATING-sweep can retry. Marking
// FAILED on a transient blip permanently strands the pod (the
// production failure shape that triggered this fix).
func TestDestroy_TransientError_StaysTerminating(t *testing.T) {
	f := &fakeRunPod{t: t, respond: func(method, path string, body []byte) (int, string) {
		if method == "DELETE" {
			return 503, `{"error":"service unavailable"}`
		}
		return 500, "{}"
	}}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	p := New(client)

	dep := okDep()
	dep.ContainerId = "rp-engine-x"

	c := &collector{}
	if err := p.Destroy(context.Background(), dep, okInst(), nil, c.emit); err == nil {
		t.Fatalf("Destroy: want error, got nil")
	}
	// Last state must be TERMINATING (not FAILED) so the reaper picks
	// up the retry. Walk the updates: there should be no FAILED emit.
	for _, u := range c.updates {
		if u.State == provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
			t.Fatalf("transient 503 emitted FAILED; expected to stay TERMINATING. updates=%+v", c.updates)
		}
	}
	if c.lastState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING {
		t.Errorf("last state = %v, want TERMINATING (so reaper retries)", c.lastState())
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
