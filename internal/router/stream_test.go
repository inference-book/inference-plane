package router

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// chunkInterval is the time the fake engine waits between SSE chunks.
// Long enough that real-time-streamed bytes are visibly different
// from end-of-response-buffered bytes; short enough that the test
// runs fast.
const chunkInterval = 50 * time.Millisecond

// streamingEngine returns an httptest.Server that emits five SSE
// chunks with chunkInterval between each. Used by streaming tests to
// assert the router forwards each chunk to the client in real-time
// (not buffered until the response closes).
func streamingEngine(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("response writer is not a Flusher; cannot stream")
			return
		}
		for i := 0; i < 5; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			fmt.Fprintf(w, "data: %s %d\n\n", prefix, i)
			flusher.Flush()
			time.Sleep(chunkInterval)
		}
		fmt.Fprintln(w, "data: [DONE]")
		flusher.Flush()
	}))
}

// TestRouter_DeployID_StreamsTokens asserts that an SSE response from
// the engine reaches the client chunk-by-chunk (each chunk arrives
// before the engine finishes). If the router buffered, the client
// would see all chunks at once at the end -- the timing assertion
// catches that.
func TestRouter_DeployID_StreamsTokens(t *testing.T) {
	engine := streamingEngine(t, "tok")
	defer engine.Close()

	r := newTestRouter(func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
			Id:             "my-llama",
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: engine.URL,
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	chunks, totalElapsed := readStreamingChunks(t, srv.URL+"/v1/my-llama/v1/chat/completions", `{"stream":true,"messages":[]}`)

	if len(chunks) < 5 {
		t.Fatalf("expected at least 5 streamed chunks, got %d: %v", len(chunks), chunks)
	}
	// The engine sleeps chunkInterval between chunks. End-of-response
	// buffering would compress all chunk-receive times to a single
	// moment; real streaming spreads them across at least
	// 4 * chunkInterval. Use a generous margin.
	if totalElapsed < 4*chunkInterval-2*chunkInterval {
		t.Errorf("chunks arrived too quickly (%v); router likely buffered the response", totalElapsed)
	}
}

// TestRouter_Flat_StreamsTokens is the same assertion via the flat
// /v1/chat/completions URL. The router buffers the request body for
// the model peek; the question is whether the response side streams.
func TestRouter_Flat_StreamsTokens(t *testing.T) {
	engine := streamingEngine(t, "flat-tok")
	defer engine.Close()

	r := newFlatTestRouter(func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
		return &provisionerv1.ListDeploymentsResponse{Deployments: []*provisionerv1.Deployment{
			{
				Id:             "any",
				Model:          "test/model",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engine.URL,
			},
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	chunks, totalElapsed := readStreamingChunks(t, srv.URL+"/v1/chat/completions", `{"model":"test/model","stream":true,"messages":[]}`)

	if len(chunks) < 5 {
		t.Fatalf("expected at least 5 streamed chunks, got %d: %v", len(chunks), chunks)
	}
	if totalElapsed < 4*chunkInterval-2*chunkInterval {
		t.Errorf("chunks arrived too quickly (%v); router likely buffered the response", totalElapsed)
	}
}

// TestRouter_ClientDisconnect_CancelsUpstream asserts the router
// propagates client disconnection to the engine: when the test
// closes its end of the response body, the engine's request
// context fires Done. Without this, killing a chat REPL mid-stream
// would leak compute on the engine until the engine's own timeout
// catches up (or never, for slow streams).
func TestRouter_ClientDisconnect_CancelsUpstream(t *testing.T) {
	upstreamCancelled := atomic.Bool{}
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintln(w, "data: first chunk")
		flusher.Flush()
		// Wait until the test cancels OR a generous timeout.
		select {
		case <-r.Context().Done():
			upstreamCancelled.Store(true)
		case <-time.After(2 * time.Second):
			t.Error("upstream context never cancelled after client disconnect")
		}
	}))
	defer engine.Close()

	r := newTestRouter(func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
			Id:             "x",
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: engine.URL,
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	// Open the request, read the first chunk, then cancel.
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		srv.URL+"/v1/x/v1/chat/completions",
		strings.NewReader(`{"stream":true,"messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	// Read just enough to know the engine started streaming.
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}

	cancel() // simulate client going away
	// Give the cancellation a moment to propagate through the
	// proxy and reach the engine handler. ReverseProxy cancels
	// the upstream request when the inbound context cancels.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if upstreamCancelled.Load() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("engine never observed client disconnect; upstream request leaked")
}

// readStreamingChunks does a POST and reads the response one line at
// a time, recording the time-since-start at which the LAST chunk
// arrives. The duration distinguishes real streaming (chunks spread
// over engine's emit delay) from buffered responses (all chunks
// land at once when the engine closes).
func readStreamingChunks(t *testing.T, url, body string) (chunks []string, totalElapsed time.Duration) {
	t.Helper()
	start := time.Now()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			if line != "" {
				chunks = append(chunks, line)
				totalElapsed = time.Since(start)
			}
		}
		if err != nil {
			break
		}
	}
	return chunks, totalElapsed
}
