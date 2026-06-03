package vast

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

// newTestProvider builds a Provider pointed at a per-test httptest.Server
// so unit tests can stub Vast.ai's REST surface without making real
// network calls. The handler dispatches by URL path; tests register
// per-path responders.
func newTestProvider(t *testing.T, handler http.Handler) (*Provider, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	client := NewClient("test-api-key", WithBaseURL(srv.URL))
	p := New(client,
		WithSSHReadyWait(100*time.Millisecond, 10*time.Millisecond),
		WithSSHProbe(func(_ context.Context, _ string, _ int32) error { return nil }),
	)
	// Pin clock for stable timestamps in assertions.
	p.clock = func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	return p, srv
}

// TestSpawn_HappyPath: search returns one offer, rent succeeds, describe
// returns a populated instance record. The returned Instance carries the
// contract id as ProviderId and the iplane id as Id.
func TestSpawn_HappyPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, r *http.Request) {
		assertAuthBearer(t, r)
		writeJSON(w, bundlesResponse{Offers: []offerSummary{
			{ID: 42, GpuName: "RTX_4090", NumGPUs: 1, DphTotal: 0.30},
		}})
	})
	mux.HandleFunc("/api/v0/asks/42/", func(w http.ResponseWriter, r *http.Request) {
		assertAuthBearer(t, r)
		if r.Method != http.MethodPut {
			t.Errorf("rent: method = %s, want PUT", r.Method)
		}
		writeJSON(w, rentResponse{Success: true, NewContract: 999})
	})
	mux.HandleFunc("/api/v0/instances/999/", func(w http.ResponseWriter, r *http.Request) {
		assertAuthBearer(t, r)
		if r.Method != http.MethodGet {
			t.Errorf("describe: method = %s, want GET", r.Method)
		}
		writeJSON(w, instanceResponse{Instances: apiInstance{
			ID:           999,
			Label:        "iplane-my-pod",
			ActualStatus: "scheduling",
			GpuName:      "RTX 4090",
			NumGPUs:      1,
			GpuRAM:       24 * 1024,
			SSHHost:      "ssh.example.vast.ai",
			SSHPort:      2222,
		}})
	})
	p, _ := newTestProvider(t, mux)

	inst, err := p.Spawn(context.Background(), &provisionerv1.Spec{
		Id:           "my-pod",
		BaseImage:    "test:latest",
		Requirements: &provisionerv1.ResourceRequirements{Sku: "RTX_4090"},
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if inst.GetProviderId() != "999" {
		t.Errorf("ProviderId = %q, want 999", inst.GetProviderId())
	}
	if inst.GetId() != "my-pod" {
		t.Errorf("Id = %q, want my-pod", inst.GetId())
	}
	if inst.GetSsh().GetHost() != "ssh.example.vast.ai" || inst.GetSsh().GetPort() != 2222 {
		t.Errorf("Ssh = %+v, want host=ssh.example.vast.ai port=2222", inst.GetSsh())
	}
	if inst.GetProvider() != "vast" {
		t.Errorf("Provider = %q, want vast", inst.GetProvider())
	}
}

// TestSpawn_NoOfferFound: search returns no offers for any SKU in the
// candidate list. Spawn surfaces an actionable error naming the
// constraints.
func TestSpawn_NoOfferFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, bundlesResponse{Offers: nil})
	})
	p, _ := newTestProvider(t, mux)

	_, err := p.Spawn(context.Background(), &provisionerv1.Spec{
		Id:           "no-offer",
		Requirements: &provisionerv1.ResourceRequirements{Sku: "RTX_4090"},
	})
	if err == nil {
		t.Fatal("expected error when no offer matches")
	}
	if !strings.Contains(err.Error(), "no rentable offer") {
		t.Errorf("error should explain 'no rentable offer': %v", err)
	}
}

// TestSpawn_RentFailureBubbles: search succeeds, rent returns
// success=false. Spawn returns a ProviderError wrapping Vast's msg.
func TestSpawn_RentFailureBubbles(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/bundles/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, bundlesResponse{Offers: []offerSummary{{ID: 7, GpuName: "RTX_4090"}}})
	})
	mux.HandleFunc("/api/v0/asks/7/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, rentResponse{Success: false, Msg: "offer no longer rentable"})
	})
	p, _ := newTestProvider(t, mux)

	_, err := p.Spawn(context.Background(), &provisionerv1.Spec{
		Id:           "race",
		Requirements: &provisionerv1.ResourceRequirements{Sku: "RTX_4090"},
	})
	if err == nil {
		t.Fatal("expected error when rent fails")
	}
	if !strings.Contains(err.Error(), "offer no longer rentable") {
		t.Errorf("error should bubble Vast's msg: %v", err)
	}
}

// TestSpawn_NoSKUMatch: requirements ask for VRAM beyond the catalog's
// largest entry; MatchSKUs returns empty. Spawn surfaces a clear
// catalog-mismatch error before any HTTP call.
func TestSpawn_NoSKUMatch(t *testing.T) {
	p, _ := newTestProvider(t, http.NotFoundHandler())
	_, err := p.Spawn(context.Background(), &provisionerv1.Spec{
		Id:           "too-big",
		Requirements: &provisionerv1.ResourceRequirements{MinVramGb: 999},
	})
	if err == nil {
		t.Fatal("expected error when no SKU satisfies constraints")
	}
	if !strings.Contains(err.Error(), "no SKU in the vast catalog") {
		t.Errorf("error should explain catalog mismatch: %v", err)
	}
}

// TestTerminate_OK: DELETE /api/v0/instances/{id} succeeds.
func TestTerminate_OK(t *testing.T) {
	called := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/instances/100/", func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodDelete {
			t.Errorf("terminate: method = %s, want DELETE", r.Method)
		}
		w.WriteHeader(http.StatusOK)
	})
	p, _ := newTestProvider(t, mux)
	if err := p.Terminate(context.Background(), "100"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if !called {
		t.Fatal("DELETE /api/v0/instances/100/ was not called")
	}
}

// TestTerminate_NotFound: a 404 response surfaces as a ProviderError
// wrapping provisioners.ErrNotFound. The Service treats this as
// success for the terminate verb (idempotent destroy).
func TestTerminate_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/instances/missing/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"msg":"not found"}`, http.StatusNotFound)
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

// TestDescribe_OK: GET /api/v0/instances/{id} returns one record;
// Describe renders it as Instance with the right shape.
func TestDescribe_OK(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v0/instances/55/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, instanceResponse{Instances: apiInstance{
			ID:           55,
			Label:        "iplane-pod-b",
			ActualStatus: "running",
			GpuName:      "RTX 4090",
			NumGPUs:      1,
			GpuRAM:       24 * 1024,
			SSHHost:      "host.vast.ai",
			SSHPort:      31337,
		}})
	})
	p, _ := newTestProvider(t, mux)

	inst, err := p.Describe(context.Background(), "55")
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if inst.GetProviderId() != "55" {
		t.Errorf("ProviderId = %q, want 55", inst.GetProviderId())
	}
	if inst.GetState() != provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE {
		t.Errorf("state = %v, want ACTIVE (vast 'running')", inst.GetState())
	}
	if inst.GetId() != "pod-b" {
		t.Errorf("Id (from label) = %q, want pod-b", inst.GetId())
	}
}

// TestList_FiltersByLabelPrefix: GET /api/v1/instances/ returns 3
// instances, 2 with iplane- prefix and 1 unrelated. List with
// label-prefix=iplane- returns just the 2.
func TestList_FiltersByLabelPrefix(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/instances/", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, instanceListResponse{Instances: []apiInstance{
			{ID: 1, Label: "iplane-foo", ActualStatus: "running"},
			{ID: 2, Label: "someone-elses-instance", ActualStatus: "running"},
			{ID: 3, Label: "iplane-bar", ActualStatus: "loading"},
		}})
	})
	p, _ := newTestProvider(t, mux)

	refs, err := p.List(context.Background(), map[string]string{"label-prefix": "iplane-"})
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

// TestMapVastState: each documented Vast actual_status maps to the
// right iplane InstanceState. The function is data-only; one test
// keeps the table honest.
func TestMapVastState(t *testing.T) {
	cases := map[string]provisionerv1.InstanceState{
		"running":     provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		"stopped":     provisionerv1.InstanceState_INSTANCE_STATE_ACTIVE,
		"scheduling":  provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
		"loading":     provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
		"created":     provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
		"":            provisionerv1.InstanceState_INSTANCE_STATE_PENDING,
		"exited":      provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED,
		"terminated":  provisionerv1.InstanceState_INSTANCE_STATE_TERMINATED,
		"offline":     provisionerv1.InstanceState_INSTANCE_STATE_FAILED,
		"failed":      provisionerv1.InstanceState_INSTANCE_STATE_FAILED,
		"weird-state": provisionerv1.InstanceState_INSTANCE_STATE_PENDING, // unknown -> PENDING
	}
	for in, want := range cases {
		if got := mapVastState(in); got != want {
			t.Errorf("mapVastState(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestMatchSKUs_VRAMThreshold: a request with min_vram_gb=40 returns
// only SKUs whose VRAM is >= 40 (no 24 GB consumer cards).
func TestMatchSKUs_VRAMThreshold(t *testing.T) {
	reqs := &provisionerv1.ResourceRequirements{MinVramGb: 40}
	got := MatchSKUs(reqs)
	if len(got) == 0 {
		t.Fatal("expected at least one match for 40 GB+")
	}
	for _, gpu := range got {
		s := LookupSKU(gpu)
		if s == nil {
			t.Errorf("matched gpu %q not in catalog", gpu)
			continue
		}
		if s.VRAMGb < 40 {
			t.Errorf("gpu %q has VRAM=%d, fails the 40 GB filter", gpu, s.VRAMGb)
		}
	}
}

// writeJSON is a tiny helper for the handler mocks.
func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// assertAuthBearer verifies every Vast request carries the bearer
// token. Catches construction bugs that would strip auth.
func assertAuthBearer(t *testing.T, r *http.Request) {
	t.Helper()
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "Bearer ") || strings.TrimPrefix(auth, "Bearer ") == "" {
		t.Errorf("missing/empty Authorization header: %q", auth)
	}
}
