package runpod

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// fakeRunPod is a single-endpoint httptest handler that captures the
// last request body for assertions and returns the canned response set
// by the test.
type fakeRunPod struct {
	t        *testing.T
	mu       string // accumulated query (sniff which mutation we got)
	lastBody map[string]any
	respond  func(query string, vars map[string]any) (status int, body string)
}

func (f *fakeRunPod) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			f.t.Errorf("missing Bearer auth header, got %q", got)
		}
		b, _ := io.ReadAll(r.Body)
		var req gqlRequest
		if err := json.Unmarshal(b, &req); err != nil {
			f.t.Errorf("decode request: %v (body=%s)", err, string(b))
			http.Error(w, "bad request", 400)
			return
		}
		f.mu = req.Query
		f.lastBody = req.Variables
		status, body := f.respond(req.Query, req.Variables)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
}

func newFake(t *testing.T, respond func(query string, vars map[string]any) (int, string)) (*fakeRunPod, *Provider) {
	t.Helper()
	f := &fakeRunPod{t: t, respond: respond}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	return f, New(client)
}

func okSpec() *provisionerv1.Spec {
	return &provisionerv1.Spec{
		Id:       "my-pod",
		Provider: provisioners.ProviderRunPod,
		Region:   "US-CA-1",
		Gpu:      &provisionerv1.GpuSpec{Class: provisioners.GPUClassSmall},
		Tags:     map[string]string{provisioners.TagID: "my-pod", provisioners.TagOperator: "default"},
	}
}

func TestProvider_Name(t *testing.T) {
	if got := New(nil).Name(); got != provisioners.ProviderRunPod {
		t.Errorf("Name() = %q, want %q", got, provisioners.ProviderRunPod)
	}
}

func TestSpawn_HappyPath(t *testing.T) {
	f, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		if !strings.Contains(query, "podFindAndDeployOnDemand") {
			t.Errorf("expected spawn mutation, got %q", query)
		}
		return 200, `{
			"data": {
				"podFindAndDeployOnDemand": {
					"id": "rp-7c8e",
					"name": "iplane-my-pod",
					"costPerHr": 0.39,
					"createdAt": "2026-05-11T18:22:11Z",
					"desiredStatus": "CREATED",
					"gpuCount": 1,
					"gpus": [{"id": "NVIDIA GeForce RTX 4090", "displayName": "RTX 4090", "memoryInGb": 24}]
				}
			}
		}`
	})
	inst, err := p.Spawn(context.Background(), okSpec())
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if inst.GetProviderId() != "rp-7c8e" {
		t.Errorf("ProviderId = %q, want rp-7c8e", inst.GetProviderId())
	}
	if inst.GetHourlyRateUsd() != 0.39 {
		t.Errorf("HourlyRateUsd = %v, want 0.39", inst.GetHourlyRateUsd())
	}
	if inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("State = %v, want ACTIVE", inst.GetState())
	}
	if inst.GetGpu().GetSku() != "NVIDIA GeForce RTX 4090" {
		t.Errorf("Sku = %q, want resolved from class small", inst.GetGpu().GetSku())
	}
	// Verify the input we sent: pod name prefix, gpu type id, env tags.
	input, ok := f.lastBody["input"].(map[string]any)
	if !ok {
		t.Fatalf("input variable missing or wrong type: %T", f.lastBody["input"])
	}
	if name, _ := input["name"].(string); name != "iplane-my-pod" {
		t.Errorf("input.name = %q, want iplane-my-pod", name)
	}
	if gid, _ := input["gpuTypeId"].(string); gid != "NVIDIA GeForce RTX 4090" {
		t.Errorf("input.gpuTypeId = %q, want NVIDIA GeForce RTX 4090", gid)
	}
	env, _ := input["env"].([]any)
	if len(env) != 2 {
		t.Errorf("input.env should have 2 tags (iplane-id, iplane-operator), got %d", len(env))
	}
}

func TestSpawn_UnknownClass(t *testing.T) {
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		t.Error("provider should reject before HTTP")
		return 200, `{"data": null}`
	})
	spec := okSpec()
	spec.Gpu.Class = "ginormous"
	_, err := p.Spawn(context.Background(), spec)
	if err == nil {
		t.Fatal("Spawn should reject unknown class before HTTP")
	}
	var pe *provisioners.ProviderError
	if !errors.As(err, &pe) {
		t.Errorf("expected *ProviderError, got %T", err)
	}
}

func TestSpawn_SkuOverridesClass(t *testing.T) {
	var sentGpuType string
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		input := vars["input"].(map[string]any)
		sentGpuType, _ = input["gpuTypeId"].(string)
		return 200, `{
			"data": {
				"podFindAndDeployOnDemand": {
					"id": "rp-xyz", "createdAt": "2026-05-11T18:22:11Z", "costPerHr": 2.49,
					"desiredStatus": "RUNNING", "gpuCount": 1
				}
			}
		}`
	})
	spec := okSpec()
	spec.Gpu = &provisionerv1.GpuSpec{Sku: "NVIDIA H100 NVL"}
	_, err := p.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if sentGpuType != "NVIDIA H100 NVL" {
		t.Errorf("gpuTypeId sent = %q, want operator-supplied SKU", sentGpuType)
	}
}

func TestSpawn_AuthFailure(t *testing.T) {
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		return 401, `{"error": "invalid api key"}`
	})
	_, err := p.Spawn(context.Background(), okSpec())
	if err == nil {
		t.Fatal("Spawn should error on 401")
	}
	var pe *provisioners.ProviderError
	if !errors.As(err, &pe) {
		t.Errorf("expected *ProviderError, got %T", err)
	}
	if pe.HTTP != 401 {
		t.Errorf("HTTP = %d, want 401", pe.HTTP)
	}
	if !strings.Contains(pe.Error(), "auth failed") {
		t.Errorf("error should mention auth, got %q", pe.Error())
	}
}

func TestSpawn_GraphQLError_PreservesMessage(t *testing.T) {
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		return 200, `{
			"errors": [{"message": "no GPU available in datacenter US-CA-1 (quota exceeded)"}]
		}`
	})
	_, err := p.Spawn(context.Background(), okSpec())
	if err == nil {
		t.Fatal("expected error from GraphQL errors[]")
	}
	if !strings.Contains(err.Error(), "no GPU available") {
		t.Errorf("error should preserve RunPod message verbatim, got %q", err.Error())
	}
}

func TestSpawn_EmptyResponse_NoCapacity(t *testing.T) {
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		return 200, `{"data": {"podFindAndDeployOnDemand": null}}`
	})
	_, err := p.Spawn(context.Background(), okSpec())
	if err == nil {
		t.Fatal("expected error when RunPod returns null pod")
	}
	if !strings.Contains(err.Error(), "no capacity") {
		t.Errorf("error should mention no capacity, got %q", err.Error())
	}
}

func TestTerminate_HappyPath(t *testing.T) {
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		if !strings.Contains(query, "podTerminate") {
			t.Errorf("expected terminate mutation, got %q", query)
		}
		input := vars["input"].(map[string]any)
		if input["podId"] != "rp-7c8e" {
			t.Errorf("podId = %v, want rp-7c8e", input["podId"])
		}
		return 200, `{"data": {"podTerminate": null}}`
	})
	if err := p.Terminate(context.Background(), "rp-7c8e"); err != nil {
		t.Errorf("Terminate: %v", err)
	}
}

func TestTerminate_NotFound_IsIdempotent(t *testing.T) {
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		return 200, `{"errors": [{"message": "Pod not found"}]}`
	})
	if err := p.Terminate(context.Background(), "rp-gone"); err != nil {
		t.Errorf("Terminate of not-found pod should be idempotent, got %v", err)
	}
}

func TestTerminate_EmptyID(t *testing.T) {
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		t.Error("provider should reject empty id before HTTP")
		return 200, "{}"
	})
	if err := p.Terminate(context.Background(), ""); err == nil {
		t.Error("Terminate should reject empty providerID")
	}
}

func TestDescribe_HappyPath(t *testing.T) {
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		if !strings.Contains(query, "query Pod") {
			t.Errorf("expected pod query, got %q", query)
		}
		return 200, `{
			"data": {
				"pod": {
					"id": "rp-7c8e",
					"name": "iplane-my-pod",
					"costPerHr": 0.39,
					"createdAt": "2026-05-11T18:22:11Z",
					"desiredStatus": "RUNNING",
					"gpuCount": 1,
					"gpus": [{"id": "NVIDIA GeForce RTX 4090", "memoryInGb": 24}],
					"env": ["iplane-id=my-pod", "iplane-operator=default"],
					"ipAddress": {"ipAddress": "1.2.3.4"}
				}
			}
		}`
	})
	inst, err := p.Describe(context.Background(), "rp-7c8e")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if inst.GetSsh().GetHost() != "1.2.3.4" {
		t.Errorf("Ssh.Host = %q, want 1.2.3.4", inst.GetSsh().GetHost())
	}
	if inst.GetGpu().GetVramGb() != 24 {
		t.Errorf("Gpu.VramGb = %d, want 24", inst.GetGpu().GetVramGb())
	}
	if inst.GetSpec().GetTags()[provisioners.TagID] != "my-pod" {
		t.Errorf("env-decoded tag iplane-id = %q, want my-pod", inst.GetSpec().GetTags()[provisioners.TagID])
	}
}

func TestDescribe_NotFound(t *testing.T) {
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		return 200, `{"data": {"pod": null}}`
	})
	_, err := p.Describe(context.Background(), "rp-gone")
	if err == nil {
		t.Fatal("Describe of missing pod should error")
	}
	if !errors.Is(err, provisioners.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestList_ServerSideNameFilter(t *testing.T) {
	f, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		return 200, `{
			"data": {
				"myself": {
					"pods": [{
						"id": "rp-7c8e",
						"name": "iplane-my-pod",
						"costPerHr": 0.39,
						"createdAt": "2026-05-11T18:22:11Z",
						"desiredStatus": "RUNNING",
						"env": ["iplane-id=my-pod", "iplane-operator=default"]
					}]
				}
			}
		}`
	})
	refs, err := p.List(context.Background(), map[string]string{
		provisioners.TagID:       "my-pod",
		provisioners.TagOperator: "default",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	input, _ := f.lastBody["input"].(map[string]any)
	if input == nil || input["name"] != "iplane-my-pod" {
		t.Errorf("server-side PodsFilter.name should be iplane-my-pod, got %v", input)
	}
	if refs[0].GetProviderState() != "RUNNING" {
		t.Errorf("ProviderState = %q, want RUNNING", refs[0].GetProviderState())
	}
	if refs[0].GetTags()[provisioners.TagID] != "my-pod" {
		t.Errorf("env-decoded tag iplane-id missing")
	}
}

func TestList_ClientSideFilter_OperatorOnly(t *testing.T) {
	// No iplane-id in filter -> no server-side name pre-filter; List
	// returns everything under the account and we filter client-side
	// by the iplane-operator env var.
	_, p := newFake(t, func(query string, vars map[string]any) (int, string) {
		return 200, `{
			"data": {
				"myself": {
					"pods": [
						{"id": "rp-mine",    "env": ["iplane-id=foo", "iplane-operator=default"], "desiredStatus": "RUNNING", "costPerHr": 0.39, "createdAt": "2026-05-11T18:22:11Z"},
						{"id": "rp-theirs",  "env": ["iplane-operator=someone-else"], "desiredStatus": "RUNNING", "costPerHr": 0.39, "createdAt": "2026-05-11T18:22:11Z"},
						{"id": "rp-no-tag",  "env": [], "desiredStatus": "RUNNING", "costPerHr": 0.39, "createdAt": "2026-05-11T18:22:11Z"}
					]
				}
			}
		}`
	})
	refs, err := p.List(context.Background(), map[string]string{
		provisioners.TagOperator: "default",
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("client-side filter should keep only rp-mine, got %d refs", len(refs))
	}
	if refs[0].GetProviderId() != "rp-mine" {
		t.Errorf("ProviderId = %q, want rp-mine", refs[0].GetProviderId())
	}
}

func TestIsActiveProviderState(t *testing.T) {
	p := New(nil)
	cases := []struct {
		state string
		want  bool
	}{
		{"RUNNING", true},
		{"running", true},
		{"CREATED", true},
		{"RESTARTING", true},
		{"EXITED", false},
		{"TERMINATED", false},
		{"PAUSED", false},
		{"", false},
	}
	for _, c := range cases {
		if got := p.IsActiveProviderState(c.state); got != c.want {
			t.Errorf("IsActiveProviderState(%q) = %v, want %v", c.state, got, c.want)
		}
	}
}

func TestResolveSKU(t *testing.T) {
	cases := []struct {
		class    string
		wantSKU  string
		wantErr  bool
	}{
		{provisioners.GPUClassSmall, "NVIDIA GeForce RTX 4090", false},
		{provisioners.GPUClassMedium, "NVIDIA RTX A6000", false},
		{provisioners.GPUClassLarge, "NVIDIA A100 80GB PCIe", false},
		{provisioners.GPUClassXLarge, "NVIDIA H100 80GB HBM3", false},
		{"colossal", "", true},
		{"", "", true},
	}
	for _, c := range cases {
		got, err := resolveSKU(c.class)
		if (err != nil) != c.wantErr {
			t.Errorf("resolveSKU(%q) err=%v wantErr=%v", c.class, err, c.wantErr)
		}
		if got != c.wantSKU {
			t.Errorf("resolveSKU(%q) = %q, want %q", c.class, got, c.wantSKU)
		}
	}
}

func TestClassifySKU(t *testing.T) {
	if got := classifySKU("NVIDIA GeForce RTX 4090"); got != provisioners.GPUClassSmall {
		t.Errorf("classifySKU(4090) = %q, want small", got)
	}
	if got := classifySKU("Custom Unknown GPU"); got != "" {
		t.Errorf("classifySKU(unknown) = %q, want empty", got)
	}
}

func TestTagsToEnvInput_RoundTrip(t *testing.T) {
	tags := map[string]string{
		provisioners.TagID:       "my-pod",
		provisioners.TagOperator: "default",
		"owner":                  "demo",
	}
	envInput := tagsToEnvInput(tags)
	if len(envInput) != 3 {
		t.Fatalf("got %d entries, want 3", len(envInput))
	}
	// Simulate RunPod's response format ("KEY=VALUE") and decode back.
	wireEnv := make([]string, 0, len(envInput))
	for _, e := range envInput {
		wireEnv = append(wireEnv, e["key"]+"="+e["value"])
	}
	decoded := envSliceToMap(wireEnv)
	for k, v := range tags {
		if decoded[k] != v {
			t.Errorf("round-trip lost tag %q: got %q want %q", k, decoded[k], v)
		}
	}
}

func TestEnvSliceToMap_SkipsMalformed(t *testing.T) {
	got := envSliceToMap([]string{"a=1", "no-equals", "=leading-equals", "b=2=with-extra"})
	if len(got) != 2 {
		t.Errorf("got %d entries, want 2 (a, b); have %v", len(got), got)
	}
	if got["a"] != "1" {
		t.Errorf("a = %q, want 1", got["a"])
	}
	if got["b"] != "2=with-extra" {
		t.Errorf("b = %q, want 2=with-extra (everything after first =)", got["b"])
	}
}
