package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/telemetry"
)

// costHarness gives the router a real cost recorder over a manual
// reader, so a test can see what one request actually emitted.
func costHarness(t *testing.T) (*metrics.CostRecorder, func() []metricdata.DataPoint[float64]) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := otel.GetMeterProvider()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(prev) })

	cr, err := metrics.NewCostRecorder(nil, nil)
	if err != nil {
		t.Fatalf("NewCostRecorder: %v", err)
	}
	return cr, func() []metricdata.DataPoint[float64] {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != telemetry.MetricInferenceActiveSeconds {
					continue
				}
				if sum, ok := m.Data.(metricdata.Sum[float64]); ok {
					return sum.DataPoints
				}
			}
		}
		return nil
	}
}

func runningDeployment(endpoint string) func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
	return func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
			Id:              "d1",
			Model:           "Qwen/Qwen2.5-32B",
			State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			InstanceIds:     []string{"i-42"},
			EngineEndpoints: []string{endpoint},
		}}, nil
	}
}

func TestServedTimeIsAttributedToTheReplicaThatServedIt(t *testing.T) {
	// The router holds the instance id for free and nothing else about
	// the instance. That is the whole design: it emits the id, the
	// control plane emits what the id means, and spend is a join. A
	// richer label here would cost a control-plane hop per request on a
	// path where that once produced a 25s p95 (#163).
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer engine.Close()

	cr, points := costHarness(t)
	r := New(&fakeDeploymentClient{describe: runningDeployment(engine.URL)}, nil, WithCostRecorder(cr))

	req := httptest.NewRequest(http.MethodPost, "/v1/d1/v1/chat/completions", http.NoBody)
	serveThroughMux(r).ServeHTTP(httptest.NewRecorder(), req)

	dps := points()
	if len(dps) != 1 {
		t.Fatalf("got %d active-seconds series, want 1", len(dps))
	}
	got := map[string]string{}
	for _, kv := range dps[0].Attributes.ToSlice() {
		got[string(kv.Key)] = kv.Value.AsString()
	}
	if got[telemetry.LabelInstanceID] != "i-42" {
		t.Errorf("instance_id = %q, want the replica that served it", got[telemetry.LabelInstanceID])
	}
	if got[telemetry.LabelDeployID] != "d1" || got[telemetry.LabelModel] != "Qwen/Qwen2.5-32B" {
		t.Errorf("labels = %+v", got)
	}
	if dps[0].Value <= 0 {
		t.Errorf("recorded %v seconds", dps[0].Value)
	}
}

func TestARequestThatNeverReachesAnEngineCostsNothing(t *testing.T) {
	// The outer request timer covers queueing and the early returns,
	// which is right for latency and wrong for cost. A deployment with
	// no reachable replica burns no card time, and counting it would
	// inflate utilization exactly when nothing was being served.
	cr, points := costHarness(t)
	describe := func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
			Id: "d1", Model: "m", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		}}, nil
	}
	r := New(&fakeDeploymentClient{describe: describe}, nil, WithCostRecorder(cr))

	req := httptest.NewRequest(http.MethodPost, "/v1/d1/v1/chat/completions", http.NoBody)
	serveThroughMux(r).ServeHTTP(httptest.NewRecorder(), req)

	if dps := points(); len(dps) != 0 {
		t.Errorf("a request with no replica recorded %d cost series", len(dps))
	}
}

func TestARouterWithNoCostRecorderStillServes(t *testing.T) {
	// Every existing test constructs the router without one, and the
	// daemon can too. A nil recorder has to be a working configuration
	// rather than a panic on the request path.
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer engine.Close()

	r := newTestRouter(runningDeployment(engine.URL))
	rec := httptest.NewRecorder()
	serveThroughMux(r).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/d1/v1/chat/completions", http.NoBody))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}
