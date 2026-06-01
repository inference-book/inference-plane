package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// flatFakeClient extends fakeDeploymentClient with a ListDeployments
// hook. The deploy-id-handler tests don't exercise ListDeployments;
// the flat handler does.
type flatFakeClient struct {
	*fakeDeploymentClient
	list func(*provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error)
}

func (f *flatFakeClient) ListDeployments(_ context.Context, req *connect.Request[provisionerv1.ListDeploymentsRequest]) (*connect.Response[provisionerv1.ListDeploymentsResponse], error) {
	resp, err := f.list(req.Msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func newFlatTestRouter(list func(*provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error)) *Router {
	return New(&flatFakeClient{
		fakeDeploymentClient: &fakeDeploymentClient{},
		list:                 list,
	})
}

func TestRouter_Flat_RunningDeployment_ReverseProxies(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("engine got %q, want /v1/chat/completions (flat URL forwards as-is)", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"model":"Qwen/Qwen2.5-7B-Instruct"`) {
			t.Errorf("engine should receive body verbatim including model field; got: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-flat-ok"}`)
	}))
	defer engine.Close()

	r := newFlatTestRouter(func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
		return &provisionerv1.ListDeploymentsResponse{Deployments: []*provisionerv1.Deployment{
			{
				Id:             "my-llama",
				Model:          "Qwen/Qwen2.5-7B-Instruct",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engine.URL,
			},
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	body := `{"model":"Qwen/Qwen2.5-7B-Instruct","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, b)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(respBody), "chatcmpl-flat-ok") {
		t.Errorf("engine response should pass through; got: %s", respBody)
	}
}

func TestRouter_Flat_MissingModel_400(t *testing.T) {
	r := newFlatTestRouter(func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
		t.Error("ListDeployments should not be called when body is missing model")
		return nil, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	assertOpenAIErrorBody(t, resp, "invalid_request_error")
}

func TestRouter_Flat_MalformedJSON_400(t *testing.T) {
	r := newFlatTestRouter(func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
		t.Error("ListDeployments should not be called for malformed JSON")
		return nil, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{not json`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRouter_Flat_ModelNotFound_404(t *testing.T) {
	r := newFlatTestRouter(func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
		// Other deployments exist but none serve the requested model.
		return &provisionerv1.ListDeploymentsResponse{Deployments: []*provisionerv1.Deployment{
			{Id: "other", Model: "different/model", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING},
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"absent/model","messages":[]}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	assertOpenAIErrorBody(t, resp, "model_not_available")
}

func TestRouter_Flat_MultipleMatches_PicksNewestRunning(t *testing.T) {
	// Engine A is older, RUNNING. Engine B is newer, RUNNING. Engine
	// C is newest but FAILED. Policy picks B (newest RUNNING).
	engineA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("engine A should not receive traffic; newer RUNNING B should win")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer engineA.Close()
	engineB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"served_by":"B"}`)
	}))
	defer engineB.Close()

	now := time.Now()
	r := newFlatTestRouter(func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
		return &provisionerv1.ListDeploymentsResponse{Deployments: []*provisionerv1.Deployment{
			{
				Id:             "older",
				Model:          "test/model",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engineA.URL,
				CreatedAt:      timestamppb.New(now.Add(-2 * time.Hour)),
			},
			{
				Id:             "newer",
				Model:          "test/model",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
				EngineEndpoint: engineB.URL,
				CreatedAt:      timestamppb.New(now.Add(-1 * time.Hour)),
			},
			{
				Id:             "newest-but-failed",
				Model:          "test/model",
				State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED,
				EngineEndpoint: "http://should-not-be-dialed",
				CreatedAt:      timestamppb.New(now),
			},
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"test/model","messages":[]}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"served_by":"B"`) {
		t.Errorf("newer RUNNING (B) should have been picked; got: %s", body)
	}
}

func TestRouter_Flat_AllMatchesNotRunning_503ish(t *testing.T) {
	// Single match in PENDING. forwardable returns 503 via the
	// shared state-to-status code path.
	r := newFlatTestRouter(func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
		return &provisionerv1.ListDeploymentsResponse{Deployments: []*provisionerv1.Deployment{
			{Id: "warming-up", Model: "test/model", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_PENDING},
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, _ := http.Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"test/model","messages":[]}`))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestRouter_Flat_CompletionsRouteAlsoForwards(t *testing.T) {
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/completions" {
			t.Errorf("engine got %q, want /v1/completions", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"id":"cmpl-flat"}`)
	}))
	defer engine.Close()

	r := newFlatTestRouter(func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
		return &provisionerv1.ListDeploymentsResponse{Deployments: []*provisionerv1.Deployment{
			{Id: "x", Model: "m", State: provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING, EngineEndpoint: engine.URL},
		}}, nil
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/completions", "application/json", strings.NewReader(`{"model":"m","prompt":"hello"}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestRouter_Flat_DeployIDRouteStillWorks(t *testing.T) {
	// Sanity: the v0.2 ch7-beat1.3 escape-hatch URL coexists with the
	// new flat URL. Same router instance, both patterns mounted.
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"served_by":"deploy-id-path"}`)
	}))
	defer engine.Close()

	r := New(&flatFakeClient{
		fakeDeploymentClient: &fakeDeploymentClient{
			describe: func(_ *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error) {
				return &provisionerv1.DescribeDeploymentResponse{Deployment: &provisionerv1.Deployment{
					Id:             "my-llama",
					State:          provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
					EngineEndpoint: engine.URL,
				}}, nil
			},
		},
		list: func(_ *provisionerv1.ListDeploymentsRequest) (*provisionerv1.ListDeploymentsResponse, error) {
			t.Error("ListDeployments should not fire when client uses the explicit-deploy-id URL")
			return nil, nil
		},
	})
	srv := httptest.NewServer(serveThroughMux(r))
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/my-llama/v1/chat/completions", "application/json", strings.NewReader(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "deploy-id-path") {
		t.Errorf("deploy-id URL should still forward; got: %s", body)
	}
}
