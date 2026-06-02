package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// TestRouter_DecisionCounter_Picked: one successful round-trip emits
// exactly one decision observation with outcome="picked" and a
// non-empty replica_id matching the chosen replica.
func TestRouter_DecisionCounter_Picked(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:              "d",
				State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint:  engine.URL, // satisfy forwardable's singular-endpoint gate
				InstanceIds:     []string{"a"},
				EngineEndpoints: []string{engine.URL},
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/d/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	dps := findCounter(t, rm, "iplane.router.decisions.total")
	if len(dps) != 1 {
		t.Fatalf("expected 1 decision observation, got %d", len(dps))
	}
	if got := attrValue(dps[0].Attributes, "outcome"); got != "picked" {
		t.Errorf("outcome = %q, want picked", got)
	}
	if got := attrValue(dps[0].Attributes, "replica_id"); got != "a" {
		t.Errorf("replica_id = %q, want a", got)
	}
	if dps[0].Value != 1 {
		t.Errorf("value = %d, want 1", dps[0].Value)
	}
}

// TestRouter_DecisionCounter_NoReplicas: a deployment whose replicas
// are all quarantined returns 503 and emits a decision with
// outcome="no_replicas" + empty replica_id.
func TestRouter_DecisionCounter_NoReplicas(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:                   "d",
				State:                provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint:       "http://stale:8000",
				InstanceIds:          []string{"a", "b"},
				EngineEndpoints:      []string{"http://a", "http://b"},
				UnhealthyInstanceIds: []string{"a", "b"},
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/d/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	dps := findCounter(t, rm, "iplane.router.decisions.total")
	if len(dps) != 1 {
		t.Fatalf("expected 1 decision observation, got %d", len(dps))
	}
	if got := attrValue(dps[0].Attributes, "outcome"); got != "no_replicas" {
		t.Errorf("outcome = %q, want no_replicas", got)
	}
	if got := attrValue(dps[0].Attributes, "replica_id"); got != "" {
		t.Errorf("replica_id = %q, want empty", got)
	}
}

// TestRouter_InFlightGauge_TracksRequest: a single request emits two
// gauge observations -- 1 on dispatch, 0 on response. The final
// observation reflects "no traffic" so the dashboard returns to
// baseline after quiescence.
func TestRouter_InFlightGauge_TracksRequest(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:              "d",
				State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint:  engine.URL,
				InstanceIds:     []string{"a"},
				EngineEndpoints: []string{engine.URL},
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/d/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	gauges := findGauge(t, rm, "iplane.replica.in_flight")
	if len(gauges) != 1 {
		t.Fatalf("expected 1 gauge data point, got %d", len(gauges))
	}
	// OTel last-value semantics: the most recent observation is what
	// the reader sees after Collect. After dispatch + response the
	// counter is back to 0; that's the value to assert on.
	if gauges[0].Value != 0 {
		t.Errorf("expected gauge to return to 0 after request completed, got %d", gauges[0].Value)
	}
	if got := attrValue(gauges[0].Attributes, "replica_id"); got != "a" {
		t.Errorf("replica_id label = %q, want a", got)
	}
	if got := attrValue(gauges[0].Attributes, "deploy_id"); got != "d" {
		t.Errorf("deploy_id label = %q, want d", got)
	}
}

// TestRouter_InFlightCounter_OscillatesUnderConcurrency: hold the
// engine until the test signals completion; while held, the in-flight
// counter for the replica reflects the held-request count. Verifies
// the counter increments before forwarding and decrements only after
// the upstream response unwinds.
func TestRouter_InFlightCounter_OscillatesUnderConcurrency(t *testing.T) {
	_, recorder := setupMetricsCapture(t)

	release := make(chan struct{})
	var seenHits atomic.Int64
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seenHits.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:              "d",
				State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint:  engine.URL,
				InstanceIds:     []string{"a"},
				EngineEndpoints: []string{engine.URL},
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	const N = 5
	var wg sync.WaitGroup
	for range N {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, _ := http.Post(srv.URL+"/v1/d/v1/chat/completions", "application/json", strings.NewReader(`{}`))
			if resp != nil {
				resp.Body.Close()
			}
		}()
	}
	// Wait for all N requests to reach the engine handler; at that
	// point each one has incremented in-flight but not yet
	// decremented (engine is held).
	for seenHits.Load() < N {
		// spin -- tests should be fast and the engine is local
	}
	counterAny, ok := r.inFlight.Load("d/a")
	if !ok {
		t.Fatalf("in-flight counter for d/a not initialized")
	}
	counter := counterAny.(*atomic.Int64)
	if got := counter.Load(); got != int64(N) {
		t.Errorf("in-flight counter during hold = %d, want %d", got, N)
	}
	close(release)
	wg.Wait()
	if got := counter.Load(); got != 0 {
		t.Errorf("in-flight counter at quiescence = %d, want 0", got)
	}
}

// TestRouter_InFlight_DoesNotTrackOnNoReplicas: the no_replicas path
// returns 503 before pickReplica produces a replica_id, so neither
// the gauge nor the trackInFlight counter should advance.
func TestRouter_InFlight_DoesNotTrackOnNoReplicas(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:                   "d",
				State:                provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint:       "http://stale:8000",
				InstanceIds:          []string{"a"},
				EngineEndpoints:      []string{"http://a"},
				UnhealthyInstanceIds: []string{"a"},
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/d/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if dps := findGauge(t, rm, "iplane.replica.in_flight"); len(dps) != 0 {
		t.Errorf("expected no in-flight gauge observations on no_replicas path, got %d", len(dps))
	}
	if _, ok := r.inFlight.Load("d/a"); ok {
		t.Errorf("in-flight counter for d/a should not be created on no_replicas path")
	}
}
