package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/telemetry"
)

// TestRouter_Affinity_HitMissLogic: recordAffinity marks a session's
// first sighting and any move as a miss, and a repeat on the same
// replica as a hit -- policy-agnostic locality accounting.
func TestRouter_Affinity_HitMissLogic(t *testing.T) {
	reader, rec := setupMetricsCapture(t)
	r := New(&fakeDeploymentClient{}, rec)
	ctx := context.Background()

	r.recordAffinity(ctx, "d", "s1", "r0") // first seen -> miss
	r.recordAffinity(ctx, "d", "s1", "r0") // same replica -> hit
	r.recordAffinity(ctx, "d", "s1", "r1") // moved -> miss

	got := affinityOutcomes(t, reader)
	if got["hit"] != 1 {
		t.Errorf("hit = %d, want 1", got["hit"])
	}
	if got["miss"] != 2 {
		t.Errorf("miss = %d, want 2", got["miss"])
	}
}

// TestRouter_Affinity_EndToEnd_HeaderDrivesHit: two requests with the
// same X-IPlane-Session through a single-replica deployment record a
// miss (turn 1) then a hit (turn 2 lands on the same replica). Proves
// the header -> recordAffinity wiring, not just the helper.
func TestRouter_Affinity_EndToEnd_HeaderDrivesHit(t *testing.T) {
	reader, rec := setupMetricsCapture(t)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
			Id: "d", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, EngineEndpoint: engine.URL,
		}}, nil
	}}, rec)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	for range 2 {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/d/v1/chat/completions", strings.NewReader(`{}`))
		req.Header.Set(SessionHeader, "sX")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
	}

	got := affinityOutcomes(t, reader)
	if got["hit"] != 1 || got["miss"] != 1 {
		t.Errorf("outcomes = %v, want hit=1 miss=1", got)
	}
}

// TestRouter_Affinity_NoSessionHeaderNoRecord: a request without
// X-IPlane-Session records no affinity outcome (nothing to key on).
func TestRouter_Affinity_NoSessionHeaderNoRecord(t *testing.T) {
	reader, rec := setupMetricsCapture(t)
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
			Id: "d", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, EngineEndpoint: engine.URL,
		}}, nil
	}}, rec)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/d/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()

	if got := affinityOutcomes(t, reader); len(got) != 0 {
		t.Errorf("recorded affinity outcomes %v for a request with no session header; want none", got)
	}
}

// affinityOutcomes collects the affinity counter into an outcome->count map.
func affinityOutcomes(t *testing.T, reader *metric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]int64{}
	for _, p := range findCounter(t, rm, telemetry.MetricRouterAffinityTotal) {
		out[attrValue(p.Attributes, telemetry.LabelOutcome)] = p.Value
	}
	return out
}
