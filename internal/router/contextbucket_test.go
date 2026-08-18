package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

func TestContextBucketBands(t *testing.T) {
	for _, tc := range []struct {
		prompt int64
		want   string
	}{
		{0, contextBucketUnknown},
		{-1, contextBucketUnknown},
		{1, "512"},
		{512, "512"},
		{513, "2k"},
		{2048, "2k"},
		{8000, "8k"},
		{8192, "8k"},
		{8193, "32k"},
		{131072, "128k"},
		{524288, "512k"},
		{524289, "1M+"},
		{1_048_576, "1M+"},
	} {
		if got := contextBucket(tc.prompt); got != tc.want {
			t.Errorf("contextBucket(%d) = %q, want %q", tc.prompt, got, tc.want)
		}
	}
}

// TestUnknownIsNotTheShortestBucket pins the distinction. An engine that
// omits prompt_tokens must not populate the 512 series, which would drag
// a cost curve toward a context length nobody ran.
func TestUnknownIsNotTheShortestBucket(t *testing.T) {
	if contextBucket(0) == contextBucket(1) {
		t.Error("a missing prompt count reports the same band as a one-token prompt")
	}
}

// TestPromptTokensFromNonStreamingBody reads the prompt length off the
// same usage block the completion count comes from.
func TestPromptTokensFromNonStreamingBody(t *testing.T) {
	tcw := newTokenCountingWriter(httptest.NewRecorder())
	tcw.Header().Set("Content-Type", "application/json")
	tcw.WriteHeader(200)
	_, _ = tcw.Write([]byte(`{"usage": {"prompt_tokens": 8000, "completion_tokens": 42}}`))

	if got := tcw.PromptTokens(); got != 8000 {
		t.Errorf("PromptTokens = %d, want 8000", got)
	}
	if got := tcw.CompletionTokens(); got != 42 {
		t.Errorf("CompletionTokens = %d, want 42 (unchanged by the prompt read)", got)
	}
}

// TestPromptTokensSurviveASplitUsageStream covers an engine that reports
// the prompt length early and the completion count on the terminal
// frame. Tracking one figure would lose the other.
func TestPromptTokensSurviveASplitUsageStream(t *testing.T) {
	tcw := newTokenCountingWriter(httptest.NewRecorder())
	tcw.Header().Set("Content-Type", "text/event-stream")
	tcw.WriteHeader(200)

	_, _ = tcw.Write([]byte("data: {\"usage\": {\"prompt_tokens\": 2000}}\n\n"))
	_, _ = tcw.Write([]byte("data: {\"choices\": [{\"delta\": {\"content\": \"hi\"}}]}\n\n"))
	_, _ = tcw.Write([]byte("data: {\"usage\": {\"completion_tokens\": 17}}\n\n"))
	_, _ = tcw.Write([]byte("data: [DONE]\n\n"))

	if got := tcw.PromptTokens(); got != 2000 {
		t.Errorf("PromptTokens = %d, want 2000 held from the early frame", got)
	}
	if got := tcw.CompletionTokens(); got != 17 {
		t.Errorf("CompletionTokens = %d, want 17 from the terminal frame", got)
	}
	if got := contextBucket(tcw.PromptTokens()); got != "2k" {
		t.Errorf("bucket = %q, want 2k", got)
	}
}

// TestMissingUsageReportsUnknown is the path a non-reporting engine takes.
func TestMissingUsageReportsUnknown(t *testing.T) {
	tcw := newTokenCountingWriter(httptest.NewRecorder())
	tcw.Header().Set("Content-Type", "application/json")
	tcw.WriteHeader(200)
	_, _ = tcw.Write([]byte(`{"choices": [{"message": {"content": "hi"}}]}`))

	if got := contextBucket(tcw.PromptTokens()); got != contextBucketUnknown {
		t.Errorf("bucket = %q, want %q", got, contextBucketUnknown)
	}
}

// TestContextBucketReachesTheTokenMetric drives a real request through the
// router and reads the label off the emitted series. The unit tests cover
// the bucketing; this covers the wiring between the response parse and
// the counter, which is where a refactor would quietly drop it.
func TestContextBucketReachesTheTokenMetric(t *testing.T) {
	reader, recorder := setupMetricsCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"usage": {"prompt_tokens": 8000, "completion_tokens": 64}}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:              "glm",
				Model:           "zai-org/GLM-5.2",
				State:           provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				InstanceIds:     []string{"i-1"},
				EngineEndpoints: []string{engine.URL},
			}}, nil
		},
	}, recorder)
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/glm/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	dps := findCounter(t, rm, "inference.tokens.generated")
	if len(dps) != 1 {
		t.Fatalf("got %d token observations, want 1", len(dps))
	}
	if got := attrValue(dps[0].Attributes, "context_bucket"); got != "8k" {
		t.Errorf("context_bucket = %q, want 8k", got)
	}
	if dps[0].Value != 64 {
		t.Errorf("token count = %d, want 64", dps[0].Value)
	}
}
