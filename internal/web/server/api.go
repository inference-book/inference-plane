// Package server wires the HTTP-side bindings on top of the in-process
// gRPC server. Two HTTP surfaces share one mux:
//
//  1. grpc-gateway routes (REST/JSON, snake_case, OpenAI-shaped error
//     envelopes) at the URLs declared in the google.api.http annotations
//     on the proto methods: /v1/completions, /v1/chat/completions,
//     /health.
//
//  2. Connect-RPC handlers at the generated paths
//     (/inferenceplane.v1.InferenceService/Complete, etc.). Connect
//     clients hit these directly with Connect protocol, gRPC clients
//     hit the same paths with HTTP/2 + protobuf.
//
// Both surfaces ultimately call the same in-process gRPC server. The
// connect adapters use a gRPC client that dials the loopback gRPC
// listener; grpc-gateway uses the same dial target via
// RegisterXxxServiceHandlerFromEndpoint. Single source of truth for
// the implementation, one code path for tests, two transport bindings
// for callers.
//
// This is the same shape used in lilbattle's web/server/api.go.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
	"github.com/inference-book/inference-plane/gen/go/inferenceplane/v1/inferenceplanev1connect"
)

// API holds the HTTP mux that serves both gateway routes and connect
// handlers. Construct with New(), then call Handler() to get the
// composed http.Handler for the entrypoint.
type API struct {
	mux     *http.ServeMux
	logger  *slog.Logger
	grpcEnd string // local gRPC server address (e.g. "127.0.0.1:9090")
}

// New constructs an API serving:
//
//	POST /v1/completions       -> InferenceService.Complete (gateway)
//	POST /v1/chat/completions  -> InferenceService.ChatComplete (gateway)
//	GET  /health               -> HealthService.Check (gateway)
//	/inferenceplane.v1.InferenceService/{Method}  -> Connect-RPC + gRPC
//	/inferenceplane.v1.HealthService/{Method}     -> Connect-RPC + gRPC
//
// grpcAddr is the address of the in-process gRPC server (typically
// 127.0.0.1:9090). The gateway dials it; the connect adapters use
// gRPC clients pointed at the same address.
func New(ctx context.Context, grpcAddr string, logger *slog.Logger) (*API, error) {
	a := &API{
		mux:     http.NewServeMux(),
		logger:  logger,
		grpcEnd: grpcAddr,
	}

	gwmux, err := a.buildGatewayMux(ctx)
	if err != nil {
		return nil, fmt.Errorf("server: gateway mux: %w", err)
	}
	// Mount gateway routes. The annotations declare /v1/* and /health,
	// so registering at "/" on the parent mux gives the gateway a
	// chance to match. Routes the gateway doesn't match return 404.
	a.mux.Handle("/", gwmux)

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

// buildGatewayMux constructs the grpc-gateway runtime mux with the
// OpenAI-shaped marshaler and error handler, then registers each
// service via RegisterXxxServiceHandlerFromEndpoint.
//
// Snake_case JSON (UseProtoNames=true) matches OpenAI's wire format.
// EmitUnpopulated=false omits zero-valued fields, matching OpenAI's
// "absent if not set" pattern.
//
// The custom error handler emits OpenAI's error envelope:
//
//	{"error": {"message": "...", "type": "...", "code": ...}}
//
// translating gRPC status codes to HTTP statuses and OpenAI's error
// type strings.
func (a *API) buildGatewayMux(ctx context.Context) (*runtime.ServeMux, error) {
	mux := runtime.NewServeMux(
		runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{
			MarshalOptions: protojson.MarshalOptions{
				UseProtoNames:   true,
				EmitUnpopulated: false,
			},
			UnmarshalOptions: protojson.UnmarshalOptions{
				DiscardUnknown: true,
			},
		}),
		runtime.WithErrorHandler(a.gatewayErrorHandler),
	)

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}
	if err := inferencev1.RegisterInferenceServiceHandlerFromEndpoint(ctx, mux, a.grpcEnd, dialOpts); err != nil {
		return nil, fmt.Errorf("register inference: %w", err)
	}
	if err := inferencev1.RegisterHealthServiceHandlerFromEndpoint(ctx, mux, a.grpcEnd, dialOpts); err != nil {
		return nil, fmt.Errorf("register health: %w", err)
	}
	return mux, nil
}

// registerConnectHandlers mounts connect-rpc handlers at their
// generated paths. Each handler wraps a gRPC client (which dials the
// same in-process gRPC server the gateway dials). Adapters convert
// connect.Request <-> gRPC bare types.
func (a *API) registerConnectHandlers() error {
	conn, err := grpc.NewClient(a.grpcEnd,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("dial gRPC: %w", err)
	}

	inferenceClient := inferencev1.NewInferenceServiceClient(conn)
	healthClient := inferencev1.NewHealthServiceClient(conn)

	inferenceAdapter := NewConnectInferenceServiceAdapter(inferenceClient)
	healthAdapter := NewConnectHealthServiceAdapter(healthClient)

	inferencePath, inferenceHandler := inferenceplanev1connect.NewInferenceServiceHandler(inferenceAdapter)
	healthPath, healthHandler := inferenceplanev1connect.NewHealthServiceHandler(healthAdapter)

	a.mux.Handle(inferencePath, inferenceHandler)
	a.mux.Handle(healthPath, healthHandler)
	return nil
}

// gatewayErrorHandler emits OpenAI-shaped error envelopes for failures
// originating in the gRPC services. Maps gRPC status codes to HTTP
// statuses and to OpenAI's error type strings ("invalid_request_error",
// "api_error", etc.) so existing OpenAI SDKs surface the right
// exception types to client code.
func (a *API) gatewayErrorHandler(
	_ context.Context,
	_ *runtime.ServeMux,
	_ runtime.Marshaler,
	w http.ResponseWriter,
	_ *http.Request,
	err error,
) {
	s := status.Convert(err)
	httpStatus, errType := codeToHTTP(s.Code())

	a.logger.Warn("gateway error", "code", s.Code().String(), "http", httpStatus, "msg", s.Message())

	w.Header().Del("Trailer")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": s.Message(),
			"type":    errType,
		},
	})
}

// codeToHTTP maps gRPC status code to HTTP status + OpenAI error type.
// Mirrors the rules from Chapter 6.5 (4xx upstream passes through,
// 5xx upstream becomes 502, ctx cancel becomes 499) but operates on
// gRPC codes that the service layer already produced.
func codeToHTTP(c codes.Code) (int, string) {
	switch c {
	case codes.OK:
		return http.StatusOK, ""
	case codes.InvalidArgument, codes.FailedPrecondition, codes.OutOfRange:
		return http.StatusBadRequest, "invalid_request_error"
	case codes.Unauthenticated:
		return http.StatusUnauthorized, "authentication_error"
	case codes.PermissionDenied:
		return http.StatusForbidden, "permission_error"
	case codes.NotFound:
		return http.StatusNotFound, "not_found_error"
	case codes.AlreadyExists:
		return http.StatusConflict, "invalid_request_error"
	case codes.ResourceExhausted:
		return http.StatusTooManyRequests, "rate_limit_error"
	case codes.Canceled:
		return 499, "client_closed_request"
	case codes.DeadlineExceeded:
		return http.StatusGatewayTimeout, "timeout_error"
	case codes.Unavailable:
		return http.StatusBadGateway, "api_error"
	case codes.Unimplemented:
		return http.StatusNotImplemented, "api_error"
	default:
		return http.StatusInternalServerError, "api_error"
	}
}
