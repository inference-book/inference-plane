// Package server wires the HTTP-side bindings on the daemon's HTTP
// listener. Three surfaces share one mux:
//
//  1. GET /health -- hand-written health probe that calls the loopback
//     HealthService.Check and returns the simple {"status":"ok"} shape
//     operators expect (see openai.go's handleHealth). We do NOT use
//     grpc-gateway here -- protojson would emit the enum as
//     "STATUS_SERVING", which doesn't match operator expectations.
//
//  2. Connect-RPC handlers at the generated paths
//     (/inferenceplane.v1.InferenceService/Complete,
//     /provisioner.v1.ProvisionerService/..., etc.). Connect clients
//     hit these directly with Connect protocol, gRPC clients hit the
//     same paths with HTTP/2 + protobuf. This is the typed surface --
//     the proto types are the contract.
//
//  3. The v0.2 data-plane router's OpenAI-compatible POST paths
//     (POST /v1/chat/completions, POST /v1/{deploy_id}/v1/...), mounted
//     via WithDataPlaneRoutes. The router owns body-peek + per-deploy
//     dispatch + streaming pass-through to the engine pod.
//
// The Inference Connect handlers still dial the loopback gRPC listener
// (single source of truth for in-process inference). The provisioner
// and deployment Connect handlers are direct adapters around the
// in-daemon *provisioners.Service per CP/DP-1 (see WithProvisionerHandler).
package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
	"github.com/inference-book/inference-plane/gen/go/inferenceplane/v1/inferenceplanev1connect"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
)

// API holds the HTTP mux that serves both the OpenAI REST surface and
// the Connect-RPC handlers. Construct with New(), then call Handler()
// to get the composed http.Handler for the entrypoint.
//
// Provisioner and deployment handlers are optional -- they mount only
// when the daemon supplies a wired Service (via WithProvisionerHandler
// and WithDeploymentHandler). v0.1 didn't expose these on the daemon
// at all; v0.2 ch7-beat1.2 turns them on inside `iplane serve`.
type API struct {
	mux                *http.ServeMux
	logger             *slog.Logger
	grpcEnd            string // local gRPC server address (e.g. "127.0.0.1:9090")
	inferenceClient    inferencev1.InferenceServiceClient
	healthClient       inferencev1.HealthServiceClient
	provisionerHandler provisionerv1connect.ProvisionerServiceHandler
	deploymentHandler  provisionerv1connect.DeploymentServiceHandler

	// dataPlaneRoutes is the deployment-routed surface mounted by the
	// daemon: pattern -> handler pairs from router.Router.Handle().
	// Each pattern is something like "POST /v1/{deploy_id}/v1/chat/completions".
	// nil = no data-plane router mounted (v0.1 mode).
	dataPlaneRoutes map[string]http.Handler
}

// Option configures optional API surfaces at construction time.
type Option func(*API)

// WithProvisionerHandler mounts the ProvisionerService Connect handler
// on /provisioner.v1.ProvisionerService/{Method}. v0.1 callers omit
// this; v0.2 daemons supply the wired Service so CLI verbs reach the
// daemon over Connect.
func WithProvisionerHandler(h provisionerv1connect.ProvisionerServiceHandler) Option {
	return func(a *API) { a.provisionerHandler = h }
}

// WithDeploymentHandler mounts the DeploymentService Connect handler
// on /provisioner.v1.DeploymentService/{Method}. Paired with
// WithProvisionerHandler; together they expose the in-daemon Service
// over the same Connect transport `iplane instance --service-url`
// already uses.
func WithDeploymentHandler(h provisionerv1connect.DeploymentServiceHandler) Option {
	return func(a *API) { a.deploymentHandler = h }
}

// WithDataPlaneRoutes mounts a set of (pattern, handler) pairs on the
// HTTP mux for the data-plane router (v0.2 ch7-beat1.3). The patterns
// use Go 1.22+ method+wildcard syntax, e.g.
// "POST /v1/{deploy_id}/v1/chat/completions"; the router package
// provides these via router.Router.Handle().
//
// This option exists to keep internal/web/server's import graph clean
// of internal/router (CP/DP-1: the data plane is its own package and
// the web server just mounts whatever handlers the daemon hands it).
func WithDataPlaneRoutes(routes map[string]http.Handler) Option {
	return func(a *API) { a.dataPlaneRoutes = routes }
}

// New constructs an API serving:
//
//	GET  /health                                  -> hand-written -> HealthService.Check
//	/inferenceplane.v1.InferenceService/{Method}  -> Connect-RPC + gRPC
//	/inferenceplane.v1.HealthService/{Method}     -> Connect-RPC + gRPC
//
// The v0.2 OpenAI-compatible POST paths (/v1/chat/completions, etc.)
// are owned by the data-plane router and mounted via
// WithDataPlaneRoutes by the daemon.
//
// grpcAddr is the address of the in-process gRPC server (typically
// 127.0.0.1:9090). The /health handler and the Inference Connect
// adapter dial it via local gRPC clients.
func New(_ context.Context, grpcAddr string, logger *slog.Logger, opts ...Option) (*API, error) {
	conn, err := grpc.NewClient(grpcAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("server: dial gRPC: %w", err)
	}

	a := &API{
		mux:             http.NewServeMux(),
		logger:          logger,
		grpcEnd:         grpcAddr,
		inferenceClient: inferencev1.NewInferenceServiceClient(conn),
		healthClient:    inferencev1.NewHealthServiceClient(conn),
	}
	for _, opt := range opts {
		opt(a)
	}

	a.registerHealthHandler()
	if err := a.registerConnectHandlers(); err != nil {
		return nil, fmt.Errorf("server: connect handlers: %w", err)
	}
	a.registerDataPlaneRoutes()
	return a, nil
}

// registerDataPlaneRoutes mounts the per-deployment router patterns
// supplied via WithDataPlaneRoutes. v0.1 callers omit the option and
// this method becomes a no-op; v0.2 daemons pass the router.Router's
// Handle() map, putting iplane back into the inference data path.
func (a *API) registerDataPlaneRoutes() {
	for pattern, h := range a.dataPlaneRoutes {
		a.mux.Handle(pattern, h)
	}
}

// Handler returns the composed http.Handler. The entrypoint wraps it
// in middleware (otelhttp, request ID, recovery, logging) before
// handing to the graceful runner.
func (a *API) Handler() http.Handler {
	return a.mux
}

// registerHealthHandler mounts GET /health. The v0.2 router owns the
// public OpenAI-compatible POST paths (/v1/chat/completions etc.) and
// mounts them via WithDataPlaneRoutes.
func (a *API) registerHealthHandler() {
	a.mux.HandleFunc("GET /health", a.handleHealth)
}

// registerConnectHandlers mounts connect-rpc handlers at their
// generated paths. Each handler wraps a gRPC client (which dials the
// same in-process gRPC server the OpenAI handlers dial). Adapters
// convert connect.Request <-> gRPC bare types.
//
// ProvisionerService and DeploymentService mount only when the daemon
// supplied handlers via WithProvisionerHandler / WithDeploymentHandler.
// These do NOT dial the in-process gRPC server; the handlers passed in
// are direct Connect adapters around the in-daemon *provisioners.Service.
// Two reasons:
//
//  1. The provisioner Service is not registered on the loopback gRPC
//     server today (v0.1 only registers Inference + Health there).
//     Adding it would mean a second source of truth for the wiring.
//  2. CP/DP-1 (CONSTRAINTS.md) puts data-plane code behind a gRPC
//     interface anyway; the daemon's own internal calls go through the
//     same handler, just without the network hop.
func (a *API) registerConnectHandlers() error {
	inferenceAdapter := NewConnectInferenceServiceAdapter(a.inferenceClient)
	healthAdapter := NewConnectHealthServiceAdapter(a.healthClient)

	inferencePath, inferenceHandler := inferenceplanev1connect.NewInferenceServiceHandler(inferenceAdapter)
	healthPath, healthHandler := inferenceplanev1connect.NewHealthServiceHandler(healthAdapter)

	a.mux.Handle(inferencePath, inferenceHandler)
	a.mux.Handle(healthPath, healthHandler)

	if a.provisionerHandler != nil {
		path, handler := provisionerv1connect.NewProvisionerServiceHandler(a.provisionerHandler)
		a.mux.Handle(path, handler)
	}
	if a.deploymentHandler != nil {
		path, handler := provisionerv1connect.NewDeploymentServiceHandler(a.deploymentHandler)
		a.mux.Handle(path, handler)
	}
	return nil
}
