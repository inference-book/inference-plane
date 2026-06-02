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

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/internal/scheduler"
)

// recordingScheduler is a no-op scheduler that records every
// submit + invokes a configurable handler synchronously. Used by
// TestRouter_WithScheduler_Swappable to prove the WithScheduler
// option installs a custom impl (acceptance criterion: "Scheduler
// can be swapped out via interface — verify by writing a no-op test
// impl").
type recordingScheduler struct {
	handler        func(context.Context, scheduler.Entry)
	routerDispatch func(context.Context, scheduler.Entry)
	mu             sync.Mutex
	calls          []recordedCall
	started        atomic.Bool
	stopped        atomic.Bool
}

type recordedCall struct {
	deploymentID string
	priority     string
}

func (s *recordingScheduler) Submit(e scheduler.Entry) error {
	s.mu.Lock()
	s.calls = append(s.calls, recordedCall{
		deploymentID: e.DeploymentID(),
		priority:     e.Priority(),
	})
	s.mu.Unlock()
	// Synchronous dispatch keeps the no-op test impl simple --
	// the router's blocking <-entry.done in enqueueOrServe still
	// waits, but the handler runs in the caller's goroutine.
	if s.handler != nil {
		s.handler(context.Background(), e)
	}
	return nil
}

func (s *recordingScheduler) Start(context.Context) { s.started.Store(true) }
func (s *recordingScheduler) Stop()                 { s.stopped.Store(true) }
func (s *recordingScheduler) Len(string) int        { return 0 }

// TestRouter_WithScheduler_Swappable: install a custom Scheduler
// via WithScheduler and verify the router routes every queued
// request through it instead of the default InteractiveFirst.
func TestRouter_WithScheduler_Swappable(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer engine.Close()

	rs := &recordingScheduler{}
	rs.handler = func(ctx context.Context, e scheduler.Entry) {
		// Delegate to dispatchEntry so the engine still sees the
		// request and the http.Post returns 200.
		// rs.router back-pointer is set below before Submit fires.
		rs.dispatch(ctx, e)
	}

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
	}, nil, WithScheduler(rs))

	rs.routerDispatch = r.dispatchEntry

	r.Start(context.Background())
	defer r.Shutdown()

	if !rs.started.Load() {
		t.Errorf("Router.Start did not call recordingScheduler.Start")
	}

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/my-llama/v1/chat/completions",
		strings.NewReader(`{}`))
	req.Header.Set(PriorityHeader, "batch")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.calls) != 1 {
		t.Fatalf("recordingScheduler saw %d Submit calls, want 1", len(rs.calls))
	}
	if rs.calls[0].deploymentID != "my-llama" {
		t.Errorf("Submit got deploymentID=%q, want my-llama", rs.calls[0].deploymentID)
	}
	if rs.calls[0].priority != "batch" {
		t.Errorf("Submit got priority=%q, want batch", rs.calls[0].priority)
	}

	r.Shutdown()
	if !rs.stopped.Load() {
		t.Errorf("Router.Shutdown did not call recordingScheduler.Stop")
	}
}

// dispatch delegates to the router-supplied handler (set after
// router construction via the routerDispatch field). Allows the
// recording scheduler's Submit to invoke the real dispatch logic
// once the router exists.
func (s *recordingScheduler) dispatch(ctx context.Context, e scheduler.Entry) {
	if s.routerDispatch != nil {
		s.routerDispatch(ctx, e)
	}
}

// routerDispatch is the dispatchEntry method bound to a Router.
// Set after router construction so the recording scheduler can
// delegate engine-bound work back to the router. Avoids a
// chicken-and-egg between Scheduler construction and Router
// construction in the test.
var _ = func() { // forces the field declaration's placement
}
