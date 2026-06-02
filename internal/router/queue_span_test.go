package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// TestRouter_QueuedPath_SpanHasQueueWaitAttr: a queued request's
// router span carries iplane.queue.wait_ms with a positive value.
// The chapter's trace-debugger narrative: span detail explains
// WHY the request took as long as it did.
func TestRouter_QueuedPath_SpanHasQueueWaitAttr(t *testing.T) {
	exp := setupTracingCapture(t)

	// Engine briefly holds the first request so the second one
	// queues behind it -- ensures a non-zero wait time when the
	// second one dispatches.
	hold := make(chan struct{})
	releaseOnce := false
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !releaseOnce {
			releaseOnce = true
			<-hold
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:             "my-llama",
				Model:          "m",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engine.URL,
			}}, nil
		},
	}, nil, WithQueue(1, 8))
	r.Start(context.Background())

	var holdClosed bool
	srv := httptest.NewServer(serveThroughMux(r))
	defer func() {
		if !holdClosed {
			close(hold)
		}
		srv.Close()
		r.Shutdown()
		engine.Close()
	}()

	// First request: lands on the held engine.
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{}`))
		if err == nil {
			resp.Body.Close()
		}
	}()

	// Give the first one time to actually enter the engine handler
	// (it's now holding on `hold`), then submit the second. The
	// second waits in the queue behind it for ~50ms minimum.
	time.Sleep(50 * time.Millisecond)
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{}`))
		if err == nil {
			resp.Body.Close()
		}
	}()

	// Let the second sit in queue for another beat, then release
	// the engine. Both requests complete.
	time.Sleep(100 * time.Millisecond)
	close(hold)
	holdClosed = true
	<-done1
	<-done2

	spans := exp.GetSpans()
	if len(spans) < 2 {
		t.Fatalf("expected >=2 spans, got %d", len(spans))
	}
	// Find a span that has the queue-wait attribute and assert it's
	// positive. The first request might race the queue-wait stamp
	// (sub-ms wait depending on goroutine scheduling); the second
	// definitely waited at least ~100ms behind the first.
	var foundPositive bool
	for _, span := range spans {
		v, ok := spanInt64AttrFromStub(span, AttrQueueWaitMs)
		if !ok {
			continue
		}
		if v > 0 {
			foundPositive = true
			break
		}
	}
	if !foundPositive {
		// Diagnostic: dump what we saw.
		for i, s := range spans {
			v, ok := spanInt64AttrFromStub(s, AttrQueueWaitMs)
			t.Logf("span[%d] %s wait=%d present=%v", i, s.Name, v, ok)
		}
		t.Fatalf("no span carried iplane.queue.wait_ms with a positive value")
	}
}

// TestRouter_DirectPath_SpanLacksQueueWaitAttr: when no scheduler is
// configured, the direct-forward path's span does NOT carry the
// queue-wait attribute. Absence is the documented signal for "this
// request didn't queue at all."
func TestRouter_DirectPath_SpanLacksQueueWaitAttr(t *testing.T) {
	exp := setupTracingCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:             "my-llama",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engine.URL,
			}}, nil
		},
	}, nil /* no WithQueue -> direct path */)

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if _, ok := spanInt64AttrFromStub(spans[0], AttrQueueWaitMs); ok {
		t.Errorf("direct-path span carries %s, want absent", AttrQueueWaitMs)
	}
}

// TestRouter_NoTenantHeader_SpanCarriesDefaultTenant: locks in the
// #81 acceptance criterion "when tenant is unset, attribute is
// `default` (not missing)." The withTenant middleware already
// defaults to DefaultTenantID; this test prevents regression.
func TestRouter_NoTenantHeader_SpanCarriesDefaultTenant(t *testing.T) {
	exp := setupTracingCapture(t)

	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	r := New(&fakeDeploymentClient{
		describe: func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
			return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
				Id:             "my-llama",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engine.URL,
			}}, nil
		},
	}, nil)

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	// No X-IPlane-Tenant header.
	resp, _ := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	resp.Body.Close()

	spans := exp.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if got := spanAttrFromStub(spans[0], AttrRouterTenantID); got != DefaultTenantID {
		t.Errorf("tenant_id = %q, want %q (default when header absent)", got, DefaultTenantID)
	}
}
