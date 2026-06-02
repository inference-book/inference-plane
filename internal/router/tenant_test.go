package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/scheduler"
)

// (setupMetricsCapture / findCounter / attrValue live in metrics_test.go;
// same package, so tenant tests reuse them without re-declaring.)

func TestExtractTenant(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"present", "alice", "alice"},
		{"trims leading", "  alice", "alice"},
		{"trims trailing", "alice  ", "alice"},
		{"trims both", "  alice  ", "alice"},
		{"empty defaults", "", DefaultTenantID},
		{"whitespace-only defaults", "   ", DefaultTenantID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tc.header != "" {
				req.Header.Set(TenantHeader, tc.header)
			}
			if got := extractTenant(req); got != tc.want {
				t.Errorf("extractTenant=%q, want %q", got, tc.want)
			}
		})
	}
}

func TestWithTenant_PutsTenantOnContext(t *testing.T) {
	var seen string
	next := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		seen = tenantFromContext(req.Context())
	})
	srv := httptest.NewServer(withTenant(next))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL, nil)
	req.Header.Set(TenantHeader, "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()
	if seen != "alice" {
		t.Errorf("ctx tenant=%q, want alice", seen)
	}
}

func TestTenantFromContext_DefaultsWhenAbsent(t *testing.T) {
	if got := tenantFromContext(context.Background()); got != DefaultTenantID {
		t.Errorf("empty ctx tenant=%q, want %q", got, DefaultTenantID)
	}
}

// TestRouter_TenantHeaderStrippedAtEngine asserts the engine receives
// the request WITHOUT the X-IPlane-Tenant header. This is the
// "engines stay tenant-agnostic" architectural invariant: tenant is
// an iplane-internal abstraction; correlation across the engine
// boundary goes through OTel trace_id, not by replicating tenant.
func TestRouter_TenantHeaderStrippedAtEngine(t *testing.T) {
	var seenHeader atomic.Pointer[string]
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get(TenantHeader)
		seenHeader.Store(&v)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{
				Deployment: &provisionerv1.Deployment{
					Id:             "my-llama",
					State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
					EngineEndpoint: engine.URL,
				},
			}, nil
		},
	}, nil)

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/my-llama/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TenantHeader, "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	got := seenHeader.Load()
	if got == nil {
		t.Fatalf("engine never received the request")
	}
	if *got != "" {
		t.Errorf("engine saw %s=%q; want stripped (empty)", TenantHeader, *got)
	}
}

// TestRouter_TenantMissingDefaults asserts a request without the
// tenant header still reaches the engine successfully and the
// engine sees no X-IPlane-Tenant. This is the default-tenant case:
// internally the router treats it as tenant="default" for metrics
// + span attrs (assertion lives in
// TestRouter_TenantThreadsIntoSpan), but the engine-facing payload
// is identical whether the header was present or absent.
func TestRouter_TenantMissingDefaults(t *testing.T) {
	var hits atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if v := r.Header.Get(TenantHeader); v != "" {
			t.Errorf("engine saw stray %s=%q on default-tenant request", TenantHeader, v)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{
				Deployment: &provisionerv1.Deployment{
					Id:             "my-llama",
					State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
					EngineEndpoint: engine.URL,
				},
			}, nil
		},
	}, nil)

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if hits.Load() != 1 {
		t.Fatalf("engine hits=%d, want 1", hits.Load())
	}
}

// TestRouter_TenantThreadsIntoMetricLabels drives a request with
// X-IPlane-Tenant: alice through the router and asserts the
// emitted metric carries tenant_id="alice". This is the
// "Beat 1 left an empty-string scaffold; Beat 2.2 fills it in"
// integration assertion.
func TestRouter_TenantThreadsIntoMetricLabels(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"completion_tokens":7}}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:             "my-llama",
				Model:          "Qwen/Qwen2.5-7B-Instruct",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engine.URL,
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/my-llama/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TenantHeader, "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	reqs := findCounter(t, rm, "inference.requests.total")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request observation, got %d", len(reqs))
	}
	if got := attrValue(reqs[0].Attributes, "tenant_id"); got != "alice" {
		t.Errorf("tenant_id label = %q, want alice", got)
	}
	tokens := findCounter(t, rm, "inference.tokens.generated")
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token observation, got %d", len(tokens))
	}
	if got := attrValue(tokens[0].Attributes, "tenant_id"); got != "alice" {
		t.Errorf("tokens tenant_id = %q, want alice", got)
	}
}

// TestRouter_TenantMissing_LabelsDefault asserts a request without
// the tenant header still produces metric observations labelled
// tenant_id="default" -- the operator-recognizable sentinel for
// unannotated traffic.
func TestRouter_TenantMissing_LabelsDefault(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage":{"completion_tokens":3}}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:             "my-llama",
				Model:          "m",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engine.URL,
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	reqs := findCounter(t, rm, "inference.requests.total")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request observation, got %d", len(reqs))
	}
	if got := attrValue(reqs[0].Attributes, "tenant_id"); got != DefaultTenantID {
		t.Errorf("tenant_id label = %q, want %q", got, DefaultTenantID)
	}
}

// TestRouter_QueuedPath_CapturesTenantOnEntry asserts the
// queued-path entry has TenantID populated from the header. The
// scheduler-facing entries that Beat 2.3+ adds will read this field
// directly without touching the request context.
func TestRouter_QueuedPath_CapturesTenantOnEntry(t *testing.T) {
	var capturedTenant atomic.Pointer[string]
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{
				Deployment: &provisionerv1.Deployment{
					Id:             "my-llama",
					State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
					EngineEndpoint: engine.URL,
				},
			}, nil
		},
	}, nil /* scheduler installed below */)

	// Install a scheduler with a wrapper handler that captures the
	// entry's TenantID before delegating to the real dispatch. With
	// no X-IPlane-Priority header (and no deployment default), the
	// effective priority resolves to INTERACTIVE, so all submits
	// land on the interactive lane. Beat 2.3's priority test covers
	// lane routing separately.
	r.scheduler = scheduler.NewInteractiveFirst(scheduler.InteractiveFirstConfig{
		Workers:             1,
		InteractiveCapacity: 4,
		BatchCapacity:       4,
		Handler: func(ctx context.Context, e scheduler.Entry) {
			se := e.(schedulerEntry).queueEntry
			v := se.TenantID
			capturedTenant.Store(&v)
			r.dispatchEntry(ctx, e)
		},
	})
	r.Start(context.Background())
	defer r.Shutdown()

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/my-llama/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(TenantHeader, "alice")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	got := capturedTenant.Load()
	if got == nil {
		t.Fatalf("queued path did not run")
	}
	if *got != "alice" {
		t.Errorf("entry.TenantID=%q, want alice", *got)
	}
}
