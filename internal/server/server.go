// Package server holds the HTTP API surface of the control plane: the
// router, OpenAI-compatible handlers, the middleware chain, and the
// /health endpoint.
//
// Middleware order (outer to inner) follows Chapter 6.5:
//   1. RequestID  -- propagate or generate an X-Request-Id; downstream
//                    layers all log against this ID.
//   2. Recovery   -- panic to 500 instead of crashing the process.
//   3. Logging    -- structured request log with status, duration, etc.
//   4. (Tracing)  -- OTel HTTP middleware  -- added in next increment.
//   5. (Metrics)  -- request counter + duration histogram -- added in next.
//
// Health checks live at /health and are excluded from request logging.
package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	skmw "github.com/panyam/servicekit/middleware"

	"github.com/inference-book/inference-plane/internal/backend"
	"github.com/inference-book/inference-plane/internal/config"
)

// Server wraps the HTTP server and the resources its handlers depend on.
type Server struct {
	cfg     *config.Config
	logger  *slog.Logger
	backend backend.Backend
	httpSrv *http.Server
}

// New builds a Server from configuration. It constructs the configured
// Backend, wires it into the request handler, registers /health with a
// ReadyFunc that probes the backend, and composes the middleware chain.
//
// Returns an error if the engine identifier in cfg is unrecognized;
// that's a config-time problem and the entrypoint should refuse to start.
func New(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	be, err := newBackendFromConfig(cfg.Backend)
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()

	// /health: servicekit's HealthCheck handler with our backend probe.
	// ReadyFunc is invoked on every probe; we wrap backend.Health with
	// a short timeout so a slow backend never blocks the health endpoint
	// past the operator's expectations.
	hc := skmw.NewHealthCheck(
		skmw.WithPath("/health"),
		skmw.WithReadyFunc(func() bool {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			return be.Health(ctx) == nil
		}),
	)
	mux.Handle(hc.Path(), hc)

	// Inference handlers. Both routes share the same handler; the
	// VLLMBackend dispatches based on whether the request body has
	// Messages set. Go 1.22+ ServeMux pattern-and-method routing.
	completions := &completionsHandler{backend: be, logger: logger}
	mux.Handle("POST /v1/completions", completions)
	mux.Handle("POST /v1/chat/completions", completions)

	// Middleware chain. Composed outer-to-inner: RequestID wraps Recovery
	// wraps RequestLogger wraps the mux. Logging skips /health so health
	// probes don't drown out real request logs at production rates.
	handler := skmw.NewRequestID().Middleware(
		skmw.Recovery(
			skmw.RequestLogger("/health")(mux),
		),
	)

	httpSrv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      handler,
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSec) * time.Second,
	}

	return &Server{
		cfg:     cfg,
		logger:  logger,
		backend: be,
		httpSrv: httpSrv,
	}, nil
}

// HTTP returns the underlying *http.Server so the entrypoint can hand
// it to servicekit's graceful runner. Exposing it (rather than wrapping
// ListenAndServe + Shutdown) keeps the lifecycle in one place: main.go.
func (s *Server) HTTP() *http.Server {
	return s.httpSrv
}

// newBackendFromConfig constructs the configured Backend. v0.1 only
// recognizes "vllm"; later versions extend this with "sglang",
// "tensorrt", "llamacpp" -- each behind the same Backend interface.
func newBackendFromConfig(cfg config.BackendConfig) (backend.Backend, error) {
	switch cfg.Engine {
	case "vllm":
		return backend.NewVLLM(cfg.Name, cfg.URL), nil
	case "":
		return nil, errors.New("server: backend.engine is required (got empty string)")
	default:
		return nil, errors.New("server: unsupported backend.engine: " + cfg.Engine)
	}
}
