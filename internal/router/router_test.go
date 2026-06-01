package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
)

// fakeDeploymentClient is the minimum surface Router needs from the
// generated DeploymentServiceClient: just DescribeDeployment. Other
// methods are unused by Router today; if Router grows new dependencies
// the compiler will surface them and we can extend the fake.
type fakeDeploymentClient struct {
	provisionerv1connect.DeploymentServiceClient // embedded so the unimplemented methods exist
	describe                                     func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error)
}

func (f *fakeDeploymentClient) DescribeDeployment(_ context.Context, req *connect.Request[provisionerv1.DescribeDeploymentRequest]) (*connect.Response[provisionerv1.DescribeDeploymentResponse], error) {
	resp, err := f.describe(req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

// newTestRouter wires a Router against a fake DescribeDeployment.
// Tests pass a describe func that returns whatever deployment record
// (or error) the case needs.
func newTestRouter(describe func(*provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error)) *Router {
	return New(&fakeDeploymentClient{describe: describe})
}

// serveThroughMux mounts the router's patterns on a fresh ServeMux so
// PathValue extraction works the same way it does in production.
// Returns a ServeMux ready to handle the test requests.
func serveThroughMux(r *Router) *http.ServeMux {
	mux := http.NewServeMux()
	for pattern, h := range r.Handle() {
		mux.Handle(pattern, h)
	}
	return mux
}

func TestRouter_RunningDeployment_ReverseProxies(t *testing.T) {
	// Engine echoes back a fixed body the test can recognize.
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("engine got unexpected path %q; deploy-id prefix should have been stripped", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-test","choices":[{"message":{"content":"hello from engine"}}]}`)
	}))
	defer engine.Close()

	r := newTestRouter(func(req *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		if req.GetId() != "my-llama" {
			t.Errorf("describe got id=%q, want my-llama", req.GetId())
		}
		return &provisionerv1.DescribeDeploymentResponse{
			Deployment: &provisionerv1.Deployment{
				Id:             "my-llama",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engine.URL,
			},
		}, nil
	})

	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "hello from engine") {
		t.Errorf("response body should pass through engine output; got: %s", body)
	}
}

func TestRouter_UnknownDeployment_404(t *testing.T) {
	r := newTestRouter(func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return nil, connect.NewError(connect.CodeNotFound, errFmt("no such deployment"))
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/missing/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	assertOpenAIErrorBody(t, resp, "deployment_not_found")
}

func TestRouter_PendingDeployment_503(t *testing.T) {
	for _, st := range []provisionerv1.DeploymentState{
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_STARTING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_CONFIGURING,
	} {
		t.Run(stateLabel(st), func(t *testing.T) {
			r := newTestRouter(func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
				return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{Id: "x", State: st}}, nil
			})
			srv := httptest.NewServer(serveThroughMux(r))
			defer srv.Close()

			resp, _ := http.Post(srv.URL+"/v1/x/v1/chat/completions", "application/json", strings.NewReader(`{}`))
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want 503", resp.StatusCode)
			}
			if resp.Header.Get("Retry-After") == "" {
				t.Error("Retry-After header should be set on a not-ready deployment")
			}
			assertOpenAIErrorBody(t, resp, "deployment_not_ready")
		})
	}
}

func TestRouter_FailedDeployment_502_WithReason(t *testing.T) {
	r := newTestRouter(func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
			Id:             "x",
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
			FailureReason:  "engine OOMed during model load",
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/x/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "engine OOMed during model load") {
		t.Errorf("failure reason should appear in error body; got: %s", body)
	}
}

func TestRouter_TerminatedDeployment_410(t *testing.T) {
	for _, st := range []provisionerv1.DeploymentState{
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
	} {
		t.Run(stateLabel(st), func(t *testing.T) {
			r := newTestRouter(func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
				return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{Id: "x", State: st}}, nil
			})
			srv := httptest.NewServer(serveThroughMux(r))
			defer srv.Close()

			resp, _ := http.Post(srv.URL+"/v1/x/v1/chat/completions", "application/json", strings.NewReader(`{}`))
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusGone {
				t.Errorf("status = %d, want 410", resp.StatusCode)
			}
			assertOpenAIErrorBody(t, resp, "deployment_gone")
		})
	}
}

func TestRouter_DegradedDeployment_502(t *testing.T) {
	r := newTestRouter(func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
			Id:    "x",
			State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_DEGRADED,
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/x/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	assertOpenAIErrorBody(t, resp, "engine_unhealthy")
}

func TestRouter_MissingEngineEndpoint_503(t *testing.T) {
	r := newTestRouter(func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
			Id:             "x",
			State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
			EngineEndpoint: "",
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/x/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestRouter_DescribeError_502(t *testing.T) {
	r := newTestRouter(func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
		return nil, connect.NewError(connect.CodeInternal, errFmt("daemon exploded"))
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/x/v1/chat/completions", "application/json", strings.NewReader(`{}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	assertOpenAIErrorBody(t, resp, "daemon_error")
}

// TestRouter_CompletionsPathAlsoForwarded asserts the non-chat
// /v1/completions route forwards too (covered by the same handler
// but registered on a different ServeMux pattern).
func TestRouter_CompletionsPathAlsoForwarded(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			t.Errorf("engine got %q, want /v1/completions", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"cmpl-test"}`)
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

	resp, err := http.Post(srv.URL+"/v1/x/v1/completions", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// assertOpenAIErrorBody decodes the OpenAI-shaped error envelope and
// asserts that the recorded error type matches. The router emits
// canonical error types (deployment_not_found, deployment_not_ready,
// etc.) so SDK-facing error handling can branch on them.
func assertOpenAIErrorBody(t *testing.T, resp *http.Response, wantType string) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	var env openAIError
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("error body should be OpenAI-shaped JSON; got: %s (parse err: %v)", body, err)
	}
	if env.Error.Type != wantType {
		t.Errorf("error type = %q, want %q (body: %s)", env.Error.Type, wantType, body)
	}
}

// errFmt is a tiny error helper. Kept local rather than pulling in
// fmt + errors.New combinations across many test cases.
type errFmt string

func (e errFmt) Error() string { return string(e) }
