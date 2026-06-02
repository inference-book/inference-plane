package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/scheduler"
)

func TestPriorityFromHeader(t *testing.T) {
	cases := []struct {
		name, header string
		want         provisionerv1.Priority
	}{
		{"interactive", "interactive", provisionerv1.Priority_PRIORITY_INTERACTIVE},
		{"interactive uppercase", "INTERACTIVE", provisionerv1.Priority_PRIORITY_INTERACTIVE},
		{"interactive whitespace", " interactive ", provisionerv1.Priority_PRIORITY_INTERACTIVE},
		{"batch", "batch", provisionerv1.Priority_PRIORITY_BATCH},
		{"batch mixed case", "Batch", provisionerv1.Priority_PRIORITY_BATCH},
		{"missing", "", provisionerv1.Priority_PRIORITY_UNSPECIFIED},
		{"unknown", "background", provisionerv1.Priority_PRIORITY_UNSPECIFIED},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			if tc.header != "" {
				req.Header.Set(PriorityHeader, tc.header)
			}
			if got := priorityFromHeader(req); got != tc.want {
				t.Errorf("priorityFromHeader=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestEffectivePriority_PrecedenceOrder(t *testing.T) {
	// Priority is request-level only -- there is no Deployment-side
	// fallback in v0.2 (engines stay priority-blind; per-deployment
	// defaults would be a routing-policy leak onto the runtime
	// artifact). Precedence: header > INTERACTIVE.
	dep := &provisionerv1.Deployment{}

	cases := []struct {
		name string
		ctx  context.Context
		want provisionerv1.Priority
	}{
		{
			name: "header sets explicit priority",
			ctx:  context.WithValue(context.Background(), priorityCtxKey{}, provisionerv1.Priority_PRIORITY_INTERACTIVE),
			want: provisionerv1.Priority_PRIORITY_INTERACTIVE,
		},
		{
			name: "header sets batch",
			ctx:  context.WithValue(context.Background(), priorityCtxKey{}, provisionerv1.Priority_PRIORITY_BATCH),
			want: provisionerv1.Priority_PRIORITY_BATCH,
		},
		{
			name: "ctx has UNSPECIFIED -> INTERACTIVE",
			ctx:  context.WithValue(context.Background(), priorityCtxKey{}, provisionerv1.Priority_PRIORITY_UNSPECIFIED),
			want: provisionerv1.Priority_PRIORITY_INTERACTIVE,
		},
		{
			name: "empty ctx (no middleware ran) -> INTERACTIVE",
			ctx:  context.Background(),
			want: provisionerv1.Priority_PRIORITY_INTERACTIVE,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectivePriority(tc.ctx, dep); got != tc.want {
				t.Errorf("effectivePriority=%v, want %v", got, tc.want)
			}
		})
	}
}

// TestRouter_PriorityHeaderStrippedAtEngine: same architectural
// invariant as the tenant header. Engines stay priority-agnostic;
// the lane is iplane's job.
func TestRouter_PriorityHeaderStrippedAtEngine(t *testing.T) {
	var seenHeader atomic.Pointer[string]
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := r.Header.Get(PriorityHeader)
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
	req.Header.Set(PriorityHeader, "batch")
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
		t.Errorf("engine saw %s=%q; want stripped", PriorityHeader, *got)
	}
}

// TestRouter_PriorityRoutesToCorrectLane verifies a batch-headered
// request lands on batchPool and an interactive request lands on
// interactivePool. Acceptance criterion: "request with
// X-IPlane-Priority: interactive lands in interactive lane."
func TestRouter_PriorityRoutesToCorrectLane(t *testing.T) {
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

	// Install a scheduler with a wrapping handler that records
	// which lane each entry hit. The wrapping handler reads the
	// entry's priority label, increments the matching counter,
	// then delegates to the real dispatch.
	var interactiveHits, batchHits atomic.Int32
	r.scheduler = scheduler.NewInteractiveFirst(scheduler.InteractiveFirstConfig{
		Workers:             2,
		InteractiveCapacity: 8,
		BatchCapacity:       8,
		Handler: func(ctx context.Context, e scheduler.Entry) {
			switch e.Priority() {
			case scheduler.LaneInteractive:
				interactiveHits.Add(1)
			case scheduler.LaneBatch:
				batchHits.Add(1)
			}
			r.dispatchEntry(ctx, e)
		},
	})
	r.Start(context.Background())
	defer r.Shutdown()

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	// Interactive request
	req1, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/my-llama/v1/chat/completions",
		strings.NewReader(`{}`))
	req1.Header.Set(PriorityHeader, "interactive")
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("interactive Do: %v", err)
	}
	resp1.Body.Close()

	// Batch request
	req2, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/my-llama/v1/chat/completions",
		strings.NewReader(`{}`))
	req2.Header.Set(PriorityHeader, "batch")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("batch Do: %v", err)
	}
	resp2.Body.Close()

	if got := interactiveHits.Load(); got != 1 {
		t.Errorf("interactive lane hits=%d, want 1", got)
	}
	if got := batchHits.Load(); got != 1 {
		t.Errorf("batch lane hits=%d, want 1", got)
	}
}

// TestRouter_PriorityFallbacksToInteractiveDefault: no header sent;
// request lands in the interactive lane (the router-level default).
// Priority is request-level only -- the engine is priority-blind,
// so there's no Deployment-side priority field driving this.
func TestRouter_PriorityFallbacksToInteractiveDefault(t *testing.T) {
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

	var interactiveHits, batchHits atomic.Int32
	r.scheduler = scheduler.NewInteractiveFirst(scheduler.InteractiveFirstConfig{
		Workers:             2,
		InteractiveCapacity: 8,
		BatchCapacity:       8,
		Handler: func(ctx context.Context, e scheduler.Entry) {
			switch e.Priority() {
			case scheduler.LaneInteractive:
				interactiveHits.Add(1)
			case scheduler.LaneBatch:
				batchHits.Add(1)
			}
			r.dispatchEntry(ctx, e)
		},
	})
	r.Start(context.Background())
	defer r.Shutdown()

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	// No X-IPlane-Priority header -- should land in the interactive
	// lane (the router-level default for unannotated requests).
	resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions",
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	resp.Body.Close()

	if interactiveHits.Load() != 1 {
		t.Errorf("interactive lane hits=%d, want 1 (unannotated requests default to interactive)", interactiveHits.Load())
	}
	if batchHits.Load() != 0 {
		t.Errorf("batch lane hits=%d, want 0 (no header, no deployment-side priority)", batchHits.Load())
	}
}

// TestRouter_LaneDepth_VisibleViaPoolLen exposes per-lane depth.
// Acceptance criterion: "Queue stats expose per-lane depth."
//
// Cleanup ordering matters here: the engine handler blocks on hold,
// so the router has in-flight requests waiting on it. If srv.Close
// (router server) fires before close(hold), it deadlocks waiting for
// those in-flight requests to drain. Single combined defer enforces
// the order: release engine first, then close server, then shutdown
// pool, then close engine. Tests that don't hold mid-handler can
// stay on the simple multi-defer pattern.
func TestRouter_LaneDepth_VisibleViaPoolLen(t *testing.T) {
	hold := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
		w.WriteHeader(http.StatusOK)
	}))

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
	}, nil, WithInteractiveQueue(1, 8), WithBatchQueue(1, 8))
	r.Start(context.Background())

	srv := httptest.NewServer(serveThroughMux(r))
	defer func() {
		close(hold)    // unblock engine handler so in-flight requests complete
		srv.Close()    // close router server
		r.Shutdown()   // drain pool servicers
		engine.Close() // close engine server
	}()

	// Submit 3 batch requests (1 in servicer, 2 queued). Interactive
	// stays empty.
	for range 3 {
		go func() {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/my-llama/v1/chat/completions",
				strings.NewReader(`{}`))
			req.Header.Set(PriorityHeader, "batch")
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}

	// Poll until batch lane reaches >= 1 (some requests queued
	// behind the in-flight one). The exact count depends on
	// goroutine scheduling; >=1 is the meaningful assertion.
	if !waitFor(func() bool { return r.scheduler.Len(scheduler.LaneBatch) >= 1 }) {
		t.Fatalf("batch lane never reached non-zero depth within poll budget")
	}
	if got := r.scheduler.Len(scheduler.LaneInteractive); got != 0 {
		t.Errorf("interactive lane depth=%d, want 0", got)
	}
}

// waitFor polls predicate up to a fixed budget (~1s). Returns true
// if predicate became true; false otherwise. Used by integration-
// style tests that need to observe an async transition.
func waitFor(predicate func() bool) bool {
	for range 100 {
		if predicate() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// TestRouter_PriorityThreadsIntoMetricLabels: verify the priority
// label is populated on the request counter.
func TestRouter_PriorityThreadsIntoMetricLabels(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
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

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/my-llama/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set(PriorityHeader, "batch")
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
	if got := attrValue(reqs[0].Attributes, "priority"); got != "batch" {
		t.Errorf("priority label = %q, want batch", got)
	}
}
