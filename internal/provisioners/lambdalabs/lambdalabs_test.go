package lambdalabs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/provisioners"
)

// newTestProvider builds a Provider pointed at a per-test
// httptest.Server so unit tests can stub Lambda's REST surface.
func newTestProvider(t *testing.T, handler http.Handler) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL))
	p := New(client,
		WithSSHReadyWait(100*time.Millisecond, 10*time.Millisecond),
		WithSSHProbe(func(_ context.Context, _ string, _ int32) error { return nil }),
	)
	p.clock = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	return p, srv
}

// TestSpawn_HappyPath: ssh-keys returns one key, launch succeeds,
// describe returns a populated record. The returned Instance
// carries the launch instance id, the resolved SKU, and the
// hourly rate from price_cents_per_hour.
func TestSpawn_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ssh-keys", func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		writeJSON(w, sshKeysResponse{Data: []struct {
			ID        string `json:"id"`
			Name      string `json:"name"`
			PublicKey string `json:"public_key"`
		}{{ID: "k1", Name: "OperatorKey", PublicKey: "ssh-rsa AAAA..."}}})
	})
	mux.HandleFunc("/api/v1/instance-operations/launch", func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		if r.Method != http.MethodPost {
			t.Errorf("launch: method = %s, want POST", r.Method)
		}
		writeJSON(w, launchResponse{Data: struct {
			InstanceIDs []string `json:"instance_ids"`
		}{InstanceIDs: []string{"inst-uuid-1"}}})
	})
	mux.HandleFunc("/api/v1/instances/inst-uuid-1", func(w http.ResponseWriter, r *http.Request) {
		assertBasicAuth(t, r)
		body := apiInstance{
			ID:     "inst-uuid-1",
			Name:   "iplane-my-pod",
			Status: "booting",
			IP:     "192.0.2.10",
		}
		body.InstanceType.Name = "gpu_1x_a10"
		body.InstanceType.PriceCentsPerHour = 129
		body.InstanceType.Specs.GPUs = 1
		body.Region.Name = "us-east-1"
		writeJSON(w, instanceResponse{Data: body})
	})
	p, _ := newTestProvider(t, mux)

	inst, err := p.Spawn(context.Background(), &provisionerv1.Spec{
		Id:           "my-pod",
		Requirements: &provisionerv1.ResourceRequirements{Sku: "gpu_1x_a10"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if inst.GetProviderId() != "inst-uuid-1" {
		t.Errorf("ProviderId = %q, want inst-uuid-1", inst.GetProviderId())
	}
	if inst.GetId() != "my-pod" {
		t.Errorf("Id = %q, want my-pod", inst.GetId())
	}
	if inst.GetSsh().GetHost() != "192.0.2.10" || inst.GetSsh().GetPort() != 22 || inst.GetSsh().GetUser() != "ubuntu" {
		t.Errorf("Ssh = %+v, want host=192.0.2.10 port=22 user=ubuntu", inst.GetSsh())
	}
	if inst.GetHourlyRateUsd() != 1.29 {
		t.Errorf("HourlyRateUsd = %v, want 1.29 (price_cents_per_hour=129)", inst.GetHourlyRateUsd())
	}
	if inst.GetProvider() != "lambdalabs" {
		t.Errorf("Provider = %q, want lambdalabs", inst.GetProvider())
	}
}

// TestSpawn_NoSSHKey: launch can't proceed without at least one SSH
// key on the account. Spawn errors with a clear pointer.
func TestSpawn_NoSSHKey(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ssh-keys", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, sshKeysResponse{Data: nil})
	})
	p, _ := newTestProvider(t, mux)
	_, err := p.Spawn(context.Background(), &provisionerv1.Spec{
		Id:           "no-key",
		Requirements: &provisionerv1.ResourceRequirements{Sku: "gpu_1x_a10"},
	})
	if err == nil {
		t.Fatal("expected error when no SSH keys exist")
	}
	if !strings.Contains(err.Error(), "no SSH keys registered") {
		t.Errorf("error should explain SSH key requirement: %v", err)
	}
}

// TestSpawn_NoSKUMatch: requirements ask for VRAM beyond the
// curated catalog; MatchSKUs returns empty. Spawn surfaces the
// catalog mismatch before any HTTP call.
func TestSpawn_NoSKUMatch(t *testing.T) {
	p, _ := newTestProvider(t, http.NotFoundHandler())
	_, err := p.Spawn(context.Background(), &provisionerv1.Spec{
		Id:           "too-big",
		Requirements: &provisionerv1.ResourceRequirements{MinVramGb: 999},
	})
	if err == nil {
		t.Fatal("expected error when no SKU satisfies constraints")
	}
	if !strings.Contains(err.Error(), "no SKU in the lambdalabs catalog") {
		t.Errorf("error should explain catalog mismatch: %v", err)
	}
}

// TestTerminate_OK verifies the instance-operations/terminate POST
// shape -- instance_ids is a list, response carries
// terminated_instances. The adapter doesn't inspect the response
// body beyond status, so the assertions focus on the request.
func TestTerminate_OK(t *testing.T) {
	called := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instance-operations/terminate", func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodPost {
			t.Errorf("terminate: method = %s, want POST", r.Method)
		}
		var body struct {
			InstanceIDs []string `json:"instance_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode terminate body: %v", err)
		}
		if len(body.InstanceIDs) != 1 || body.InstanceIDs[0] != "inst-x" {
			t.Errorf("instance_ids = %v, want [inst-x]", body.InstanceIDs)
		}
		writeJSON(w, map[string]any{"data": map[string]any{"terminated_instances": []any{}}})
	})
	p, _ := newTestProvider(t, mux)
	if err := p.Terminate(context.Background(), "inst-x"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if !called {
		t.Fatal("terminate endpoint was not called")
	}
}

// TestTerminate_NotFound: 404 returned from Lambda surfaces as
// ProviderError wrapping ErrNotFound (idempotent destroy contract).
func TestTerminate_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instance-operations/terminate", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":"global/object-does-not-exist","message":"Instance not found"}}`, http.StatusNotFound)
	})
	p, _ := newTestProvider(t, mux)
	err := p.Terminate(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected NotFound error")
	}
	if !errors.Is(err, provisioners.ErrNotFound) {
		t.Errorf("error should wrap ErrNotFound: %v", err)
	}
}

// TestDescribe_OK: GET /api/v1/instances/{id} returns a typed
// record; Describe renders it with the right shape.
func TestDescribe_OK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances/inst-2", func(w http.ResponseWriter, _ *http.Request) {
		body := apiInstance{
			ID:     "inst-2",
			Name:   "iplane-pod-b",
			Status: "active",
			IP:     "192.0.2.11",
		}
		body.InstanceType.Name = "gpu_1x_a100_sxm4"
		body.InstanceType.PriceCentsPerHour = 199
		body.InstanceType.Specs.GPUs = 1
		body.Region.Name = "us-west-2"
		writeJSON(w, instanceResponse{Data: body})
	})
	p, _ := newTestProvider(t, mux)

	inst, err := p.Describe(context.Background(), "inst-2")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if inst.GetProviderId() != "inst-2" {
		t.Errorf("ProviderId = %q, want inst-2", inst.GetProviderId())
	}
	if inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("state = %v, want ACTIVE (lambda 'active')", inst.GetState())
	}
	if inst.GetId() != "pod-b" {
		t.Errorf("Id (from name) = %q, want pod-b", inst.GetId())
	}
	if inst.GetRegion() != "us-west-2" {
		t.Errorf("Region = %q, want us-west-2", inst.GetRegion())
	}
}

// TestList_FiltersByNamePrefix: GET /api/v1/instances returns 3
// instances, 2 with iplane- prefix and 1 unrelated. List with
// name-prefix=iplane- returns just the 2.
func TestList_FiltersByNamePrefix(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances", func(w http.ResponseWriter, _ *http.Request) {
		body := []apiInstance{
			{ID: "1", Name: "iplane-foo", Status: "active"},
			{ID: "2", Name: "someone-elses-instance", Status: "active"},
			{ID: "3", Name: "iplane-bar", Status: "booting"},
		}
		writeJSON(w, instanceListResponse{Data: body})
	})
	p, _ := newTestProvider(t, mux)

	refs, err := p.List(context.Background(), map[string]string{"name-prefix": "iplane-"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("len(refs) = %d, want 2", len(refs))
	}
	gotIDs := []string{refs[0].GetTags()[provisioners.TagID], refs[1].GetTags()[provisioners.TagID]}
	wantSet := map[string]bool{"foo": false, "bar": false}
	for _, id := range gotIDs {
		if _, ok := wantSet[id]; ok {
			wantSet[id] = true
		} else {
			t.Errorf("unexpected iplane id in list: %q", id)
		}
	}
	for id, seen := range wantSet {
		if !seen {
			t.Errorf("missing expected iplane id: %q", id)
		}
	}
}

// TestMapLambdaState: every documented Lambda status maps to the
// right iplane state.
func TestMapLambdaState(t *testing.T) {
	cases := map[string]provisionerv1.InstanceState{
		"active":      provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		"unhealthy":   provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		"booting":     provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
		"":            provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
		"terminating": provisionerv1.InstanceState_INSTANCE_STATE_TERMINATING,
		"terminated":  provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED,
		"weird-state": provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
	}
	for in, want := range cases {
		if got := mapLambdaState(in); got != want {
			t.Errorf("mapLambdaState(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestMatchSKUs_VRAMThreshold: a request with min_vram_gb=80
// returns only large-tier SKUs (A100 SXM4, H100 PCIe, H100 SXM5,
// GH200, B200) -- not the 24 GB A10 or 40 GB A100 PCIe.
func TestMatchSKUs_VRAMThreshold(t *testing.T) {
	reqs := &provisionerv1.ResourceRequirements{MinVramGb: 80}
	got := MatchSKUs(reqs)
	if len(got) == 0 {
		t.Fatal("expected at least one match for 80 GB+")
	}
	for _, name := range got {
		s := LookupSKU(name)
		if s == nil {
			t.Errorf("matched SKU %q not in catalog", name)
			continue
		}
		if s.VRAMGb < 80 {
			t.Errorf("SKU %q has VRAM=%d, fails the 80 GB filter", name, s.VRAMGb)
		}
	}
}

// writeJSON is a tiny helper for the handler mocks.
func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// assertBasicAuth verifies every Lambda request carries Basic
// auth with the API key as the username. Catches construction
// bugs that would strip auth.
func assertBasicAuth(t *testing.T, r *http.Request) {
	t.Helper()
	user, _, ok := r.BasicAuth()
	if !ok {
		t.Errorf("missing Basic auth header")
		return
	}
	if user == "" {
		t.Errorf("Basic auth user (API key) is empty")
	}
}
