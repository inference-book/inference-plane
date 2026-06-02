package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/metrics"
)

// setupMetricsCapture wires the global MeterProvider to a manual
// reader so tests can introspect the emitted metrics. Returns the
// reader (Collect into a metricdata.ResourceMetrics) and a recorder
// built against the freshly-wired provider.
//
// Each test gets a fresh provider so observations don't bleed across
// cases. The provider replaces otel's global until the test ends.
func setupMetricsCapture(t *testing.T) (*metric.ManualReader, *metrics.Recorder) {
	t.Helper()
	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	t.Cleanup(func() {
		otel.SetMeterProvider(prev)
	})
	rec, err := metrics.NewRecorder()
	if err != nil {
		t.Fatalf("metrics.NewRecorder: %v", err)
	}
	return reader, rec
}

// findCounter walks a ResourceMetrics looking for a counter
// instrument by name. Returns the matching data points (one per
// distinct attribute set) so tests can assert label values.
func findCounter(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.DataPoint[int64] {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q is not a Sum[int64]: %T", name, m.Data)
			}
			return sum.DataPoints
		}
	}
	return nil
}

// findHistogram is the histogram analogue of findCounter.
func findHistogram(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			h, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %q is not a Histogram[float64]: %T", name, m.Data)
			}
			return h.DataPoints
		}
	}
	return nil
}

// findGauge is the synchronous-gauge analogue of findCounter,
// returning observations as Int64 data points. Used for the
// iplane.replica.in_flight gauge from #88.
func findGauge(t *testing.T, rm metricdata.ResourceMetrics, name string) []metricdata.DataPoint[int64] {
	t.Helper()
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != name {
				continue
			}
			g, ok := m.Data.(metricdata.Gauge[int64])
			if !ok {
				t.Fatalf("metric %q is not a Gauge[int64]: %T", name, m.Data)
			}
			return g.DataPoints
		}
	}
	return nil
}

// attrValue extracts the string value of attr key from a data point.
func attrValue(set attribute.Set, key string) string {
	if v, ok := set.Value(attribute.Key(key)); ok {
		return v.AsString()
	}
	return ""
}

// TestRouter_DeployID_RecordsMetrics drives one successful request
// through the deploy-id URL and asserts the router emitted the
// expected counter + histogram + token observations with the right
// labels. End-to-end via httptest + a real metric reader so we catch
// any mistake in the recorder wiring as well as in the router.
func TestRouter_DeployID_RecordsMetrics(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-mt","usage":{"completion_tokens":11}}`)
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

	resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	// Requests counter
	reqs := findCounter(t, rm, "inference.requests.total")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request observation, got %d", len(reqs))
	}
	if got := reqs[0].Value; got != 1 {
		t.Errorf("requests counter = %d, want 1", got)
	}
	if got := attrValue(reqs[0].Attributes, "deploy_id"); got != "my-llama" {
		t.Errorf("deploy_id label = %q, want my-llama", got)
	}
	if got := attrValue(reqs[0].Attributes, "model"); got != "Qwen/Qwen2.5-7B-Instruct" {
		t.Errorf("model label = %q, want Qwen/Qwen2.5-7B-Instruct", got)
	}
	if got := attrValue(reqs[0].Attributes, "status"); got != "success" {
		t.Errorf("status label = %q, want success", got)
	}

	// Duration histogram
	hist := findHistogram(t, rm, "inference.request.duration")
	if len(hist) != 1 {
		t.Fatalf("expected 1 duration observation, got %d", len(hist))
	}
	if hist[0].Count != 1 {
		t.Errorf("duration histogram count = %d, want 1", hist[0].Count)
	}

	// Tokens counter
	tokens := findCounter(t, rm, "inference.tokens.generated")
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token observation, got %d", len(tokens))
	}
	if got := tokens[0].Value; got != 11 {
		t.Errorf("tokens counter = %d, want 11", got)
	}
}

// TestRouter_Flat_RecordsMetricsForStreaming verifies the streaming
// path: a SSE response with a usage frame produces a token-count
// observation matching the engine's reported count.
func TestRouter_Flat_RecordsMetricsForStreaming(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		// Two delta frames + a usage frame + the [DONE] sentinel.
		for _, line := range []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n",
			"data: {\"choices\":[{\"delta\":{\"content\":\" there\"}}]}\n\n",
			"data: {\"choices\":[{\"finish_reason\":\"stop\"}],\"usage\":{\"completion_tokens\":23}}\n\n",
			"data: [DONE]\n\n",
		} {
			_, _ = io.WriteString(w, line)
			flusher.Flush()
		}
	}))
	defer engine.Close()

	r := New(&flatFakeClient{
		fakeDeploymentClient: &fakeDeploymentClient{},
		list: func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
			return &provisionerv1.ListDeploymentsResponse{Deployments: []*provisionerv1.Deployment{
				{Id: "streamer", Model: "test/model", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, EngineEndpoint: engine.URL},
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"test/model","stream":true,"messages":[]}`))
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	tokens := findCounter(t, rm, "inference.tokens.generated")
	if len(tokens) != 1 {
		t.Fatalf("expected 1 streaming token observation, got %d", len(tokens))
	}
	if got := tokens[0].Value; got != 23 {
		t.Errorf("streaming token count = %d, want 23 (from usage frame)", got)
	}
}

// TestRouter_RecordsMetricsForErrorOutcomes verifies the
// status-label mapping for non-success outcomes: a PENDING
// deployment generates an "client_error"-shaped failure (503 ->
// "engine_error" by our mapping; reviewer note: 503 falls in the 5xx
// bucket per the chosen convention). No token emission for non-200s.
func TestRouter_RecordsMetricsForErrorOutcomes(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	r := New(&fakeDeploymentClient{
		describe: func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:    "warmup",
				Model: "test/model",
				State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/warmup/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	reqs := findCounter(t, rm, "inference.requests.total")
	if len(reqs) != 1 {
		t.Fatalf("expected 1 request observation, got %d", len(reqs))
	}
	if got := attrValue(reqs[0].Attributes, "status"); got != "engine_error" {
		t.Errorf("status label = %q, want engine_error (503 maps to 5xx bucket)", got)
	}

	tokens := findCounter(t, rm, "inference.tokens.generated")
	if len(tokens) != 0 {
		t.Errorf("expected 0 token observations on error, got %d", len(tokens))
	}
}
