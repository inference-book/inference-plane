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

// fakeRunPod is an httptest handler that dispatches on (method, path)
// and records the last request so tests can assert what we sent.
type fakeRunPod struct {
	t          *testing.T
	mu         struct{}
	lastMethod string
	lastPath   string
	lastQuery  string
	lastBody   []byte
	respond    func(method, path string, body []byte) (status int, response string)
}

func (f *fakeRunPod) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			f.t.Errorf("missing Bearer auth header, got %q", got)
		}
		f.lastMethod = r.Method
		f.lastPath = r.URL.Path
		f.lastQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		f.lastBody = b
		status, body := f.respond(r.Method, r.URL.Path, b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	})
}

func newFake(t *testing.T, respond func(method, path string, body []byte) (int, string)) (*fakeRunPod, *Provider) {
	t.Helper()
	f := &fakeRunPod{t: t, respond: respond}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL), WithHTTPClient(srv.Client()))
	return f, New(client)
}

func okSpec() *provisionerv1.Spec {
	// The Service is responsible for expanding class shorthand into
	// constraints before the adapter sees the spec; in tests we set
	// the expanded constraints directly so the adapter call site
	// gets exercised at face value.
	return &provisionerv1.Spec{
		Id:       "my-pod",
		Provider: provisioners.ProviderRunPod,
		Region:   "US-CA-1",
		Requirements: &provisionerv1.ResourceRequirements{
			MinVramGb: 24,
			MinDiskGb: 20,
			MinRamGb:  16,
		},
	}
}

func TestProvider_Name(t *testing.T) {
	if got := New(nil).Name(); got != provisioners.ProviderRunPod {
		t.Errorf("Name() = %q, want %q", got, provisioners.ProviderRunPod)
	}
}

func TestSpawn_HappyPath(t *testing.T) {
	var sentBody map[string]any
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		switch {
		case method == "POST" && path == "/pods":
			_ = json.Unmarshal(body, &sentBody)
			return 201, `{"id": "rp-7c8e", "desiredStatus": "RUNNING", "status": "running"}`
		case method == "GET" && path == "/pods/rp-7c8e":
			return 200, `{
				"id": "rp-7c8e",
				"name": "iplane-my-pod",
				"image": "runpod/pytorch:2.4.0",
				"costPerHr": 0.39,
				"createdAt": "2026-05-11T18:22:11Z",
				"desiredStatus": "RUNNING",
				"machine": {
					"gpuTypeId": "NVIDIA GeForce RTX 4090",
					"gpuCount": 1,
					"dataCenterId": "US-CA-1",
					"gpuType": {"id": "NVIDIA GeForce RTX 4090", "memoryInGb": 24}
				}
			}`
		}
		t.Errorf("unexpected request %s %s", method, path)
		return 500, "{}"
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
	if inst.GetGpu().GetSku() != "NVIDIA RTX A5000" {
		t.Errorf("Sku = %q, want cheapest 24GB SKU (A5000)", inst.GetGpu().GetSku())
	}
	if inst.GetGpu().GetVramGb() != 24 {
		t.Errorf("VramGb = %d, want 24", inst.GetGpu().GetVramGb())
	}
	if inst.GetRegion() != "US-CA-1" {
		t.Errorf("Region = %q, want US-CA-1", inst.GetRegion())
	}
	// Verify what we sent.
	if name, _ := sentBody["name"].(string); name != "iplane-my-pod" {
		t.Errorf("POST body name = %q, want iplane-my-pod", name)
	}
	ids, _ := sentBody["gpuTypeIds"].([]any)
	if len(ids) == 0 || ids[0] != "NVIDIA RTX A5000" {
		t.Errorf("POST body gpuTypeIds = %v, want [NVIDIA RTX A5000, ...] (cheapest 24GB SKU first)", ids)
	}
	dcs, _ := sentBody["dataCenterIds"].([]any)
	if len(dcs) == 0 || dcs[0] != "US-CA-1" {
		t.Errorf("POST body dataCenterIds = %v, want [US-CA-1]", dcs)
	}
	cloud, _ := sentBody["cloudType"].(string)
	if cloud != "SECURE" {
		t.Errorf("POST body cloudType = %q, want SECURE", cloud)
	}
}

func TestSpawn_PassesFullSKUFallbackList(t *testing.T) {
	var sentBody map[string]any
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/pods" {
			_ = json.Unmarshal(body, &sentBody)
			return 201, `{"id": "rp-xyz"}`
		}
		if method == "GET" && path == "/pods/rp-xyz" {
			return 200, `{"id": "rp-xyz", "name": "iplane-my-pod", "createdAt": "2026-05-11T18:22:11Z"}`
		}
		t.Errorf("unexpected request %s %s", method, path)
		return 500, "{}"
	})
	// large-class constraints expanded: ≥80 GB VRAM, ≥60 GB disk, ≥64 GB RAM.
	spec := okSpec()
	spec.Requirements = &provisionerv1.ResourceRequirements{
		MinVramGb: 80,
		MinDiskGb: 60,
		MinRamGb:  64,
	}
	if _, err := p.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	ids, _ := sentBody["gpuTypeIds"].([]any)
	if len(ids) < 2 {
		t.Errorf("gpuTypeIds should carry every SKU satisfying the constraint, got %v", ids)
	}
}

func TestSpawn_NoMatchingSKU(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		t.Error("provider should reject before HTTP")
		return 500, "{}"
	})
	// Asking for more VRAM than any SKU in the runpod catalog provides
	// (no GPU in the table has > 96 GB VRAM).
	spec := okSpec()
	spec.Requirements = &provisionerv1.ResourceRequirements{MinVramGb: 200}
	_, err := p.Spawn(context.Background(), spec)
	if err == nil {
		t.Fatal("Spawn should reject when no SKU satisfies the constraint")
	}
	var pe *provisioners.ProviderError
	if !errors.As(err, &pe) {
		t.Errorf("expected *ProviderError, got %T", err)
	}
}

func TestSpawn_SkuOverridesClass(t *testing.T) {
	var sentBody map[string]any
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method == "POST" && path == "/pods" {
			_ = json.Unmarshal(body, &sentBody)
			return 201, `{"id": "rp-xyz"}`
		}
		if method == "GET" && path == "/pods/rp-xyz" {
			return 200, `{"id": "rp-xyz", "name": "iplane-my-pod", "createdAt": "2026-05-11T18:22:11Z"}`
		}
		t.Errorf("unexpected request %s %s", method, path)
		return 500, "{}"
	})
	spec := okSpec()
	spec.Requirements = &provisionerv1.ResourceRequirements{Sku: "NVIDIA H100 NVL"}
	if _, err := p.Spawn(context.Background(), spec); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	ids, _ := sentBody["gpuTypeIds"].([]any)
	if len(ids) != 1 || ids[0] != "NVIDIA H100 NVL" {
		t.Errorf("gpuTypeIds = %v, want [NVIDIA H100 NVL] only", ids)
	}
}

func TestSpawn_AuthFailure(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 401, `{"error": "invalid api key"}`
	})
	_, err := p.Spawn(context.Background(), okSpec())
	if err == nil {
		t.Fatal("Spawn should error on 401")
	}
	var pe *provisioners.ProviderError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *ProviderError, got %T", err)
	}
	if pe.HTTP != 401 {
		t.Errorf("HTTP = %d, want 401", pe.HTTP)
	}
	if !strings.Contains(pe.Error(), "auth failed") {
		t.Errorf("error should mention auth, got %q", pe.Error())
	}
}

func TestSpawn_ErrorPreservesMessage(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 400, `{"error": "no GPU available in datacenter US-CA-1 (quota exceeded)"}`
	})
	_, err := p.Spawn(context.Background(), okSpec())
	if err == nil {
		t.Fatal("expected error from 400")
	}
	if !strings.Contains(err.Error(), "no GPU available") {
		t.Errorf("error should preserve RunPod message verbatim, got %q", err.Error())
	}
}

func TestSpawn_EmptyId_NoCapacity(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 201, `{"id": "", "desiredStatus": ""}`
	})
	_, err := p.Spawn(context.Background(), okSpec())
	if err == nil {
		t.Fatal("expected error when RunPod returns empty id")
	}
	if !strings.Contains(err.Error(), "no capacity") {
		t.Errorf("error should mention no capacity, got %q", err.Error())
	}
}

func TestSpawn_FollowupFailure_FallsBackToMinimalInstance(t *testing.T) {
	// Spawn succeeded at POST; GET /pods/{id} fails (e.g., 500). The
	// adapter should still return an Instance the Service can record,
	// because the pod really does exist in RunPod and refusing now
	// would leak it.
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method == "POST" {
			return 201, `{"id": "rp-7c8e"}`
		}
		return 500, `{"error": "transient"}`
	})
	inst, err := p.Spawn(context.Background(), okSpec())
	if err != nil {
		t.Fatalf("Spawn should swallow follow-up GET failure: %v", err)
	}
	if inst.GetProviderId() != "rp-7c8e" {
		t.Errorf("ProviderId = %q, want rp-7c8e", inst.GetProviderId())
	}
	if inst.GetGpu().GetSku() != "NVIDIA RTX A5000" {
		t.Errorf("fallback should carry the resolved SKU; got %q", inst.GetGpu().GetSku())
	}
}

func TestTerminate_HappyPath(t *testing.T) {
	f, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method != "DELETE" {
			t.Errorf("expected DELETE, got %s", method)
		}
		if path != "/pods/rp-7c8e" {
			t.Errorf("path = %q, want /pods/rp-7c8e", path)
		}
		return 204, ""
	})
	if err := p.Terminate(context.Background(), "rp-7c8e"); err != nil {
		t.Errorf("Terminate: %v", err)
	}
	_ = f
}

func TestTerminate_NotFound_IsIdempotent(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 404, `{"error": "Pod not found"}`
	})
	if err := p.Terminate(context.Background(), "rp-gone"); err != nil {
		t.Errorf("Terminate of not-found pod should be idempotent, got %v", err)
	}
}

func TestTerminate_EmptyID(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		t.Error("provider should reject empty id before HTTP")
		return 500, "{}"
	})
	if err := p.Terminate(context.Background(), ""); err == nil {
		t.Error("Terminate should reject empty providerID")
	}
}

func TestDescribe_HappyPath(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method != "GET" || path != "/pods/rp-7c8e" {
			t.Errorf("expected GET /pods/rp-7c8e, got %s %s", method, path)
		}
		return 200, `{
			"id": "rp-7c8e",
			"name": "iplane-my-pod",
			"costPerHr": 0.39,
			"createdAt": "2026-05-11T18:22:11Z",
			"desiredStatus": "RUNNING",
			"publicIp": "1.2.3.4",
			"machine": {
				"gpuTypeId": "NVIDIA GeForce RTX 4090",
				"gpuCount": 1,
				"dataCenterId": "US-CA-1",
				"gpuType": {"id": "NVIDIA GeForce RTX 4090", "memoryInGb": 24}
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
		t.Errorf("name-decoded tag iplane-id = %q, want my-pod", inst.GetSpec().GetTags()[provisioners.TagID])
	}
}

func TestDescribe_NotFound(t *testing.T) {
	_, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 404, `{"error": "Pod not found"}`
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
	f, p := newFake(t, func(method, path string, body []byte) (int, string) {
		if method != "GET" || path != "/pods" {
			t.Errorf("expected GET /pods, got %s %s", method, path)
		}
		return 200, `[{
			"id": "rp-7c8e",
			"name": "iplane-my-pod",
			"costPerHr": 0.39,
			"createdAt": "2026-05-11T18:22:11Z",
			"desiredStatus": "RUNNING"
		}]`
	})
	refs, err := p.List(context.Background(), map[string]string{provisioners.TagID: "my-pod"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d refs, want 1", len(refs))
	}
	if !strings.Contains(f.lastQuery, "name=iplane-my-pod") {
		t.Errorf("server-side name filter not applied; got query %q", f.lastQuery)
	}
	if refs[0].GetProviderState() != "RUNNING" {
		t.Errorf("ProviderState = %q, want RUNNING", refs[0].GetProviderState())
	}
	if refs[0].GetTags()[provisioners.TagID] != "my-pod" {
		t.Errorf("name-decoded tag iplane-id missing")
	}
}

func TestList_NoFilter_ReturnsAll(t *testing.T) {
	f, p := newFake(t, func(method, path string, body []byte) (int, string) {
		return 200, `[
			{"id": "rp-mine", "name": "iplane-foo", "createdAt": "2026-05-11T18:22:11Z", "desiredStatus": "RUNNING"},
			{"id": "rp-other", "name": "other-name", "createdAt": "2026-05-11T18:22:11Z", "desiredStatus": "RUNNING"}
		]`
	})
	refs, err := p.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 {
		t.Errorf("got %d refs, want 2", len(refs))
	}
	if f.lastQuery != "" {
		t.Errorf("expected no query for empty filter, got %q", f.lastQuery)
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

func TestMatchSKUs(t *testing.T) {
	// VRAM constraints should narrow to the SKUs that satisfy them,
	// cheapest first.
	cases := []struct {
		name     string
		reqs     *provisionerv1.ResourceRequirements
		wantFirst string // expected first SKU (cheapest matching)
		wantNonEmpty bool
	}{
		{"24GB VRAM", &provisionerv1.ResourceRequirements{MinVramGb: 24}, "NVIDIA RTX A5000", true},
		{"80GB VRAM", &provisionerv1.ResourceRequirements{MinVramGb: 80}, "NVIDIA A100 80GB PCIe", true},
		{"200GB VRAM (above all SKUs)", &provisionerv1.ResourceRequirements{MinVramGb: 200}, "", false},
		{"40GB+128RAM (H100 PCIe is cheapest with 128 GB system RAM)", &provisionerv1.ResourceRequirements{MinVramGb: 40, MinRamGb: 128}, "NVIDIA H100 PCIe", true},
	}
	for _, c := range cases {
		got := MatchSKUs(c.reqs)
		if c.wantNonEmpty && len(got) == 0 {
			t.Errorf("%s: got empty, want at least one match", c.name)
			continue
		}
		if !c.wantNonEmpty && len(got) != 0 {
			t.Errorf("%s: got %v, want empty", c.name, got)
			continue
		}
		if c.wantNonEmpty && got[0] != c.wantFirst {
			t.Errorf("%s: first SKU = %q, want %q (cheapest matching)", c.name, got[0], c.wantFirst)
		}
	}
}

func TestMatchSKUs_CappedAtMaxSKUsPerRequest(t *testing.T) {
	// class=small expands to min_vram_gb=24, which matches >10 SKUs in
	// the catalog. The cap prevents us from sending the full list to
	// RunPod (which would include B200, H200, H100 NVL etc. and risk
	// account-level 401s plus silent price-tier upgrades).
	got := MatchSKUs(&provisionerv1.ResourceRequirements{MinVramGb: 24})
	if len(got) > MaxSKUsPerRequest {
		t.Errorf("got %d SKUs (%v), want <= %d (cap)", len(got), got, MaxSKUsPerRequest)
	}
	// The cap should keep the cheapest matches; B200 at $5.99/hr should
	// not be in the list when class=small.
	for _, sku := range got {
		if sku == "NVIDIA B200" || sku == "NVIDIA H200" {
			t.Errorf("class=small returned frontier SKU %q in the gpuTypeIds list; cap should keep only cheap matches", sku)
		}
	}
}

func TestParseErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "array form with problems prefers problems",
			body: `[{"error":"generic envelope","problems":["At /pods/properties/dataCenterIds/items/enum: value must be one of 'US-WA-1', ..."]}]`,
			want: "At /pods/properties/dataCenterIds/items/enum: value must be one of 'US-WA-1', ...",
		},
		{
			name: "array form without problems falls back to error",
			body: `[{"error":"Pod not found"}]`,
			want: "Pod not found",
		},
		{
			name: "single-object form",
			body: `{"error":"invalid api key"}`,
			want: "invalid api key",
		},
		{
			name: "single-object message variant",
			body: `{"message":"rate limited"}`,
			want: "rate limited",
		},
		{
			name: "multiple problems joined",
			body: `[{"problems":["field A wrong","field B missing"]}]`,
			want: "field A wrong; field B missing",
		},
		{
			name: "unrecognized shape returns raw body",
			body: `<html>500 Internal Server Error</html>`,
			want: "<html>500 Internal Server Error</html>",
		},
	}
	for _, c := range cases {
		got := parseErrorMessage([]byte(c.body))
		if got != c.want {
			t.Errorf("%s:\n  body: %s\n  got:  %q\n  want: %q", c.name, c.body, got, c.want)
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
