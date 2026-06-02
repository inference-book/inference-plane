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
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// TestRouter_Queued_PathDispatches verifies that with WithQueue
// configured, requests still flow through the router → engine path
// and produce the same response shape as the direct path. The
// regression target is wiring (pool/queue plumbing): the queue must
// not corrupt the request body, response writer, or headers.
func TestRouter_Queued_PathDispatches(t *testing.T) {
	var engineHits atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		engineHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"hi"}}]}`)
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
	}, nil, WithQueue(2, 8))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer r.Shutdown()

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if got := engineHits.Load(); got != 1 {
		t.Fatalf("engine hits=%d, want 1 (queue dropped or duplicated request)", got)
	}
}

// TestRouter_Queued_BoundedConcurrency holds k engine handlers
// simultaneously and asserts that a (k+1)th submission cannot reach
// the engine until one of the in-flight ones completes — that's the
// k-servicer cap. Verifies the M/M/k shape end-to-end through the
// router.
func TestRouter_Queued_BoundedConcurrency(t *testing.T) {
	const servicers = 2
	const capacity = 8

	hold := make(chan struct{})
	var inFlight atomic.Int32
	var peak atomic.Int32
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		current := inFlight.Add(1)
		for {
			old := peak.Load()
			if current <= old || peak.CompareAndSwap(old, current) {
				break
			}
		}
		<-hold
		inFlight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
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
	}, nil, WithQueue(servicers, capacity))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer r.Shutdown()

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	// Fire (servicers + 2) requests concurrently. Only `servicers`
	// should be in flight at the engine at once; the extras wait in
	// the queue.
	const total = servicers + 2
	var wg sync.WaitGroup
	results := make(chan int, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
			if err != nil {
				results <- 0
				return
			}
			defer resp.Body.Close()
			io.Copy(io.Discard, resp.Body)
			results <- resp.StatusCode
		}()
	}

	// Wait until exactly `servicers` are in flight (the cap).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if inFlight.Load() == int32(servicers) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := inFlight.Load(); got != int32(servicers) {
		close(hold)
		wg.Wait()
		t.Fatalf("inFlight=%d, want %d after burst", got, servicers)
	}
	// Give the queue a beat to settle.
	time.Sleep(20 * time.Millisecond)
	if got := inFlight.Load(); got != int32(servicers) {
		close(hold)
		wg.Wait()
		t.Fatalf("inFlight grew past %d to %d (engine concurrency cap broken)", servicers, got)
	}

	close(hold)
	wg.Wait()
	close(results)

	for code := range results {
		if code != http.StatusOK {
			t.Errorf("client got status %d, want 200", code)
		}
	}
	if got := peak.Load(); got != int32(servicers) {
		t.Errorf("peak in-flight at engine=%d, want %d (M/M/k cap violated)", got, servicers)
	}
}

// TestRouter_Queued_FullReturns503 saturates the engine + queue and
// asserts the next submission returns 503 with Retry-After. This is
// the chapter's bounded-buffer backpressure narrative.
func TestRouter_Queued_FullReturns503(t *testing.T) {
	const servicers = 1
	const capacity = 1

	hold := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
		w.WriteHeader(http.StatusOK)
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
	}, nil, WithQueue(servicers, capacity))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r.Start(ctx)
	defer r.Shutdown()

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	// Submit 2 requests in flight: one in the servicer (held), one
	// queued. Both block.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
			if err == nil {
				resp.Body.Close()
			}
		}(i)
	}

	// Give the burst time to fully land (1 popped, 1 queued).
	time.Sleep(50 * time.Millisecond)

	// Third request must hit the full-queue backpressure.
	resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		close(hold)
		wg.Wait()
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		close(hold)
		wg.Wait()
		t.Fatalf("status=%d (want 503), body=%s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Retry-After"); got == "" {
		close(hold)
		wg.Wait()
		t.Fatalf("missing Retry-After header on full-queue 503")
	}

	close(hold)
	wg.Wait()
}

// TestRouter_NoQueue_StaysOnDirectPath documents that the default
// Router (no WithQueue, no Start) preserves the Beat 1 direct-forward
// path. Regression target: pool field defaults to nil; enqueueOrServe
// falls through to handleWithObservability without queue overhead.
func TestRouter_NoQueue_StaysOnDirectPath(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
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
	}, nil /* no WithQueue */)
	if r.pool != nil {
		t.Fatalf("expected no pool on default Router; got %v", r.pool)
	}

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
}

// TestRouter_Shutdown_DrainsInFlight starts the pool, kicks off a
// long-running request, calls Shutdown, and verifies the in-flight
// request completes before Shutdown returns.
func TestRouter_Shutdown_DrainsInFlight(t *testing.T) {
	hold := make(chan struct{})
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-hold
		w.WriteHeader(http.StatusOK)
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
	}, nil, WithQueue(1, 4))
	r.Start(context.Background())

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	clientDone := make(chan struct{})
	go func() {
		resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{}`))
		if err == nil {
			resp.Body.Close()
		}
		close(clientDone)
	}()

	// Wait for the engine to receive the in-flight request.
	time.Sleep(50 * time.Millisecond)

	shutdownDone := make(chan struct{})
	go func() {
		// Note: Shutdown blocks while servicers drain. Release the
		// engine's hold so the in-flight handler completes; Shutdown
		// should observe the servicer return and exit.
		close(hold)
		r.Shutdown()
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("Shutdown did not return within 2s")
	}
	select {
	case <-clientDone:
	case <-time.After(time.Second):
		t.Fatalf("client did not get response within 1s of Shutdown")
	}
}

