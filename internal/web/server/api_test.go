package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/router"
	"github.com/inference-book/inference-plane/internal/web/server"
)

// stubProvisionerHandler embeds the generated Unimplemented stub so
// every RPC returns CodeUnimplemented. The smoke test never calls
// these — it only exercises route registration on the shared mux.
type stubProvisionerHandler struct {
	provisionerv1connect.UnimplementedProvisionerServiceHandler
}

type stubDeploymentHandler struct {
	provisionerv1connect.UnimplementedDeploymentServiceHandler
}

// TestNew_BootsWithDaemonOptions guards against the regression where
// iplane serve panicked on startup because a legacy hand-coded handler
// registered the same pattern as the v0.2 router's flat-URL handler.
//
// Wires server.New the way cmd/iplane/cmd/serve.go does (Provisioner +
// Deployment Connect handlers + the real router's Handle() map) and
// asserts:
//
//   - server.New does not panic on mux pattern registration
//   - GET /health resolves (hand-coded handler)
//   - POST /provisioner.v1.DeploymentService/ListDeployments resolves
//     (Connect-RPC handlers register)
//   - POST /v1/chat/completions resolves (router's flat URL is not
//     eclipsed or duplicated by another registration)
//   - POST /v1/{deploy_id}/v1/chat/completions resolves
//
// "Resolves" = response status is not 404. Request handling itself is
// not exercised; the goal is to catch wiring regressions, not behavior
// regressions.
func TestNew_BootsWithDaemonOptions(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	recorder, err := metrics.NewRecorder()
	if err != nil {
		t.Fatalf("metrics.NewRecorder: %v", err)
	}

	deployClient := provisionerv1connect.NewDeploymentServiceClient(
		http.DefaultClient,
		"http://127.0.0.1:0",
	)
	r := router.New(deployClient, recorder)

	api, err := server.New(
		ctx,
		"127.0.0.1:0",
		logger,
		server.WithProvisionerHandler(stubProvisionerHandler{}),
		server.WithDeploymentHandler(stubDeploymentHandler{}),
		server.WithDataPlaneRoutes(r.Handle()),
	)
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ts := httptest.NewServer(api.Handler())
	defer ts.Close()

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			name:   "GET /health",
			method: http.MethodGet,
			path:   "/health",
		},
		{
			name:   "POST DeploymentService/ListDeployments",
			method: http.MethodPost,
			path:   "/provisioner.v1.DeploymentService/ListDeployments",
			body:   "{}",
		},
		{
			name:   "POST /v1/chat/completions (router flat URL)",
			method: http.MethodPost,
			path:   "/v1/chat/completions",
			body:   `{"model":"x"}`,
		},
		{
			name:   "POST /v1/x/v1/chat/completions (router deploy-id URL)",
			method: http.MethodPost,
			path:   "/v1/x/v1/chat/completions",
			body:   `{}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, ts.URL+tc.path, strings.NewReader(tc.body))
			if err != nil {
				t.Fatalf("NewRequest: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusNotFound {
				t.Fatalf("got 404 for %s %s — route not registered", tc.method, tc.path)
			}
		})
	}
}
