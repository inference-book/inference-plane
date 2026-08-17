package vast

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// The Service routes a provider away from the sshdocker executor only when
// it satisfies this interface. Vast rents containers, so being image-native
// is the whole point of the adapter; signature drift would silently send it
// back down the docker-in-docker path that cannot work.
var _ provisioners.Deployer = (*Provider)(nil)

func engineDep() *provisionerv1.Deployment {
	return &provisionerv1.Deployment{
		Id:               "d1",
		Image:            "vllm/vllm-openai:v0.7.0",
		Model:            "Qwen/Qwen2.5-0.5B-Instruct",
		EnginePort:       8000,
		EngineEntrypoint: []string{"python3", "-m", "vllm.entrypoints.openai.api_server"},
		EngineArgs:       []string{"--max-model-len", "4096"},
	}
}

func engineInst() *provisionerv1.Instance {
	return &provisionerv1.Instance{
		Id:       "d1",
		Provider: "vast",
		Spec: &provisionerv1.Spec{
			Requirements: &provisionerv1.ResourceRequirements{Sku: "RTX_3060", GpuCount: 1, MinDiskGb: 60},
		},
	}
}

func collect() (func(provisioners.DeployStateUpdate), *[]provisioners.DeployStateUpdate) {
	var got []provisioners.DeployStateUpdate
	return func(u provisioners.DeployStateUpdate) { got = append(got, u) }, &got
}

// Vast replaces the image's entrypoint with its own launcher, so an image
// that would have started itself on RunPod starts nothing here. Failing
// early with a message naming the flag beats renting a machine that runs
// sshd and nothing else.
func TestDeployRequiresAnEngineEntrypoint(t *testing.T) {
	p, _ := newTestProvider(t, http.NotFoundHandler())
	dep := engineDep()
	dep.EngineEntrypoint = nil
	emit, updates := collect()

	err := p.Deploy(t.Context(), dep, engineInst(), nil, emit)

	if err == nil {
		t.Fatal("want an error when engine_entrypoint is missing")
	}
	if !strings.Contains(err.Error(), "engine_entrypoint") {
		t.Errorf("error = %v, want it to name the missing field", err)
	}
	if len(*updates) == 0 || (*updates)[len(*updates)-1].State != provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED {
		t.Error("failure was not emitted as a terminal state")
	}
}

// The engine argv is quoted because a model name or an operator's engine
// arg is an arbitrary string, and this text becomes a shell script on a
// machine we are paying for.
func TestOnstartScriptQuotesEveryArgument(t *testing.T) {
	dep := engineDep()
	dep.Model = "org/model; rm -rf /"
	dep.EngineArgs = []string{"--flag", "value with spaces"}

	got := onstartScript(dep.GetEngineEntrypoint(), dep, 8000)

	if !strings.HasPrefix(got, "exec ") {
		t.Errorf("script does not exec the engine: %q", got)
	}
	if !strings.Contains(got, `'org/model; rm -rf /'`) {
		t.Errorf("model name was not quoted: %q", got)
	}
	if !strings.Contains(got, `'value with spaces'`) {
		t.Errorf("engine arg was not quoted: %q", got)
	}
	if !strings.Contains(got, `'--port' '8000'`) {
		t.Errorf("engine port not passed: %q", got)
	}
}

func TestOnstartScriptEscapesEmbeddedQuotes(t *testing.T) {
	dep := engineDep()
	dep.EngineArgs = []string{"it's"}

	if got := onstartScript(dep.GetEngineEntrypoint(), dep, 8000); !strings.Contains(got, `'it'\''s'`) {
		t.Errorf("embedded quote not escaped: %q", got)
	}
}

// Vast has no typed ports field; a mapping is requested through the env map
// using docker's own "-p" spelling. Getting this wrong yields an engine
// nothing outside the box can reach.
func TestRentEngineRequestsThePortMapping(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"success":true,"new_contract":99}`))
	}))
	defer srv.Close()
	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))

	if _, err := p.rentEngine(t.Context(), 42, engineDep(), engineDep().GetEngineEntrypoint(), 8000, 60); err != nil {
		t.Fatalf("rentEngine: %v", err)
	}

	env, _ := body["env"].(map[string]any)
	if _, ok := env["-p 8000:8000"]; !ok {
		t.Errorf("env does not request the port mapping: %v", env)
	}
	if body["image"] != "vllm/vllm-openai:v0.7.0" {
		t.Errorf("image = %v, want the engine image", body["image"])
	}
	onstart, _ := body["onstart"].(string)
	if !strings.Contains(onstart, "vllm.entrypoints.openai.api_server") {
		t.Errorf("onstart does not start the engine: %q", onstart)
	}
}

// The endpoint is not derivable up front the way RunPod's proxy URL is, so
// an unmapped port must read as "not yet" rather than as a usable address.
func TestEngineEndpointWaitsForTheMapping(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"no public address", map[string]any{"id": 1}, ""},
		{"address but no mapping", map[string]any{"id": 1, "public_ipaddr": "1.2.3.4"}, ""},
		{"mapped", map[string]any{
			"id": 1, "public_ipaddr": "1.2.3.4",
			"ports": map[string]any{"8000/tcp": []map[string]string{{"HostIp": "0.0.0.0", "HostPort": "41234"}}},
		}, "http://1.2.3.4:41234"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"instances": tc.body})
			}))
			defer srv.Close()
			p, _ := newTestProvider(t, nil)
			p.client = NewClient("k", WithBaseURL(srv.URL))

			got, note := p.engineEndpoint(t.Context(), 1, 8000)
			if got != tc.want {
				t.Errorf("endpoint = %q, want %q (note: %s)", got, tc.want, note)
			}
			if tc.want == "" && note == "" {
				t.Error("no endpoint and no reason; the operator would see a silent wait")
			}
		})
	}
}

// Readiness is the engine answering, not the container existing. A mapped
// port that 503s is a model still loading.
func TestWaitForEngineReadyPollsUntilHealthy(t *testing.T) {
	var healthHits int
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		healthHits++
		if healthHits < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer engine.Close()
	host, port := splitHostPort(t, engine.URL)

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": map[string]any{
			"id": 1, "public_ipaddr": host,
			"ports": map[string]any{"8000/tcp": []map[string]string{{"HostPort": port}}},
		}})
	}))
	defer api.Close()

	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(api.URL))
	p.engineReadyTimeout = 5 * time.Second
	p.sshReadyInterval = 10 * time.Millisecond
	emit, _ := collect()

	got, err := p.waitForEngineReady(t.Context(), 1, 8000, emit)
	if err != nil {
		t.Fatalf("waitForEngineReady: %v", err)
	}
	if got == "" {
		t.Error("no endpoint returned on success")
	}
	if healthHits < 3 {
		t.Errorf("health hits = %d; it returned before the engine was serving", healthHits)
	}
}

func TestWaitForEngineReadyTimesOutWithAReason(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"instances": map[string]any{"id": 1}})
	}))
	defer api.Close()
	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(api.URL))
	p.engineReadyTimeout = 40 * time.Millisecond
	p.sshReadyInterval = 5 * time.Millisecond
	emit, _ := collect()

	_, err := p.waitForEngineReady(t.Context(), 1, 8000, emit)
	if err == nil {
		t.Fatal("want a timeout error")
	}
	if !strings.Contains(err.Error(), "public address") && !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("error = %v, want it to say what it was waiting for", err)
	}
}

func TestDestroyWithNoContractIsNotAnError(t *testing.T) {
	p, _ := newTestProvider(t, http.NotFoundHandler())
	emit, updates := collect()

	if err := p.Destroy(t.Context(), &provisionerv1.Deployment{Id: "d1"}, nil, nil, emit); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	last := (*updates)[len(*updates)-1]
	if last.State != provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		t.Errorf("state = %v, want TERMINATED", last.State)
	}
}

// The two ids differ by deployment shape: the singular path stamps the
// deployment, the multi-replica path stamps each instance. Reading only one
// is how issue 228 leaked every non-slot-0 machine.
func TestDestroyFallsBackToTheInstanceProviderID(t *testing.T) {
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted = r.URL.Path
		}
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()
	p, _ := newTestProvider(t, nil)
	p.client = NewClient("k", WithBaseURL(srv.URL))
	emit, _ := collect()

	err := p.Destroy(t.Context(), &provisionerv1.Deployment{Id: "d1"},
		&provisionerv1.Instance{Id: "d1", ProviderId: "12345"}, nil, emit)
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if !strings.Contains(deleted, "12345") {
		t.Errorf("deleted path = %q, want the instance's contract id", deleted)
	}
}

func splitHostPort(t *testing.T, url string) (string, string) {
	t.Helper()
	trimmed := strings.TrimPrefix(url, "http://")
	i := strings.LastIndex(trimmed, ":")
	if i < 0 {
		t.Fatalf("cannot split %q", url)
	}
	return trimmed[:i], trimmed[i+1:]
}

// Vast must not claim to attach mounts, because its deploy path does not
// read Deployment.mounts at all. Before #254 the claim was implicit:
// nothing asked, so a configured warm cache was accepted and dropped,
// the model downloaded, and the deploy still reported
// storage_tier=warm.
//
// Delete this test when Vast learns to attach one. The seam is the env
// map rentEngine already uses to pass docker run options ("-p 8000:8000"
// is set that way), so a host_path bind would ride the same channel.
// Volumes proper stay out of reach while they are machine-scoped, which
// is the rest of #254.
func TestVastDoesNotClaimToAttachMounts(t *testing.T) {
	var p provisioners.Provider = &Provider{}
	if ma, ok := p.(provisioners.MountAttacher); ok && ma.AttachesMounts() {
		t.Error("vast declares it attaches mounts, but its deployer never reads dep.GetMounts()")
	}
}
