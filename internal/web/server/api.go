// Package server wires the HTTP-side bindings on top of the in-process
// gRPC server. Two HTTP surfaces share one mux:
//
//  1. OpenAI-compatible REST/JSON at the wire shape OpenAI documents:
//     POST /v1/completions, POST /v1/chat/completions, GET /health.
//     Hand-written http.Handlers (see openai.go) decode/encode the
//     JSON. We do NOT use grpc-gateway here -- protojson serializes
//     int64 fields as JSON strings and enum values as their proto
//     names ("STATUS_SERVING"), neither of which matches OpenAI's
//     wire format. Owning the translation explicitly keeps the public
//     contract honest.
//
//  2. Connect-RPC handlers at the generated paths
//     (/inferenceplane.v1.InferenceService/Complete, etc.). Connect
//     clients hit these directly with Connect protocol, gRPC clients
//     hit the same paths with HTTP/2 + protobuf. This is the typed
//     surface -- the proto types are the contract.
//
// Both surfaces ultimately call the same in-process gRPC server. The
// connect adapters and the OpenAI handlers both use a gRPC client that
// dials the loopback gRPC listener. Single source of truth for the
// implementation, two transport bindings for callers, with hand-coded
// JSON shaping for the OpenAI public surface.
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
)

// API holds the HTTP mux that serves both the OpenAI REST surface and
// the Connect-RPC handlers. Construct with New(), then call Handler()
// to get the composed http.Handler for the entrypoint.
type API struct {
	mux             *http.ServeMux
	logger          *slog.Logger
	grpcEnd         string // local gRPC server address (e.g. "127.0.0.1:9090")
	inferenceClient inferencev1.InferenceServiceClient
	healthClient    inferencev1.HealthServiceClient
}

// New constructs an API serving:
//
//	POST /v1/completions       -> OpenAI handler -> InferenceService.Complete
//	POST /v1/chat/completions  -> OpenAI handler -> InferenceService.ChatComplete
//	GET  /health               -> OpenAI handler -> HealthService.Check
//	/inferenceplane.v1.InferenceService/{Method}  -> Connect-RPC + gRPC
//	/inferenceplane.v1.HealthService/{Method}     -> Connect-RPC + gRPC
//
// grpcAddr is the address of the in-process gRPC server (typically
// 127.0.0.1:9090). Both the OpenAI handlers and the connect adapters
// dial it via local gRPC clients.
func New(_ context.Context, grpcAddr string, logger *slog.Logger) (*API, error) {
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

	a.registerOpenAIHandlers()
	if err := a.registerConnectHandlers(); err != nil {
		return nil, fmt.Errorf("server: connect handlers: %w", err)
	}
	return a, nil
}

// Handler returns the composed http.Handler. The entrypoint wraps it
// in middleware (otelhttp, request ID, recovery, logging) before
// handing to the graceful runner.
func (a *API) Handler() http.Handler {
	return a.mux
}

// registerOpenAIHandlers mounts the public OpenAI-compatible REST
// routes. Method+path patterns require Go 1.22+; the more specific
// patterns take precedence over any catch-all.
func (a *API) registerOpenAIHandlers() {
	a.mux.HandleFunc("GET /health", a.handleHealth)
	a.mux.HandleFunc("POST /v1/completions", a.handleComplete)
	a.mux.HandleFunc("POST /v1/chat/completions", a.handleChatComplete)
}

// registerConnectHandlers mounts connect-rpc handlers at their
// generated paths. Each handler wraps a gRPC client (which dials the
// same in-process gRPC server the OpenAI handlers dial). Adapters
// convert connect.Request <-> gRPC bare types.
func (a *API) registerConnectHandlers() error {
	inferenceAdapter := NewConnectInferenceServiceAdapter(a.inferenceClient)
	healthAdapter := NewConnectHealthServiceAdapter(a.healthClient)

	inferencePath, inferenceHandler := inferenceplanev1connect.NewInferenceServiceHandler(inferenceAdapter)
	healthPath, healthHandler := inferenceplanev1connect.NewHealthServiceHandler(healthAdapter)

	a.mux.Handle(inferencePath, inferenceHandler)
	a.mux.Handle(healthPath, healthHandler)
	return nil
}
