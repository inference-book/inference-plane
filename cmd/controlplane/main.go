// Command controlplane is the v0.1 entrypoint.
//
// Loads config from YAML + env, initializes the OpenTelemetry SDK,
// constructs the backend and the gRPC service implementations, mounts
// connect-rpc handlers + the OpenAI-compatible HTTP routes on a
// single HTTP mux, wraps the chain with otelhttp + servicekit
// middleware, and runs the lifecycle via servicekit's graceful
// runner.
//
// One process, one port (8080), two protocol surfaces:
//
//	/inferenceplane.v1.InferenceService/{Method}  -- Connect-RPC + gRPC
//	/inferenceplane.v1.HealthService/{Method}     -- Connect-RPC + gRPC
//	/v1/completions                                -- OpenAI HTTP
//	/v1/chat/completions                           -- OpenAI HTTP
//	/health                                        -- HTTP (also Connect-RPC)
//
// The OpenAI routes call into the connect handlers in-process; no
// loopback HTTP, no extra serialization round-trip.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	skhttp "github.com/panyam/servicekit/http"
	skmw "github.com/panyam/servicekit/middleware"

	"github.com/inference-book/inference-plane/gen/go/inferenceplane/v1/inferenceplanev1connect"
	"github.com/inference-book/inference-plane/internal/backends"
	"github.com/inference-book/inference-plane/internal/config"
	"github.com/inference-book/inference-plane/internal/services"
	"github.com/inference-book/inference-plane/internal/telemetry"
	"github.com/inference-book/inference-plane/internal/web/openai"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(os.Getenv("CP_CONFIG_PATH"))
	if err != nil {
		logger.Error("config load failed", "err", err)
		os.Exit(1)
	}

	shutdownTel, err := telemetry.Init(context.Background(), cfg.Telemetry, cfg.Deployment)
	if err != nil {
		logger.Error("telemetry init failed", "err", err)
		os.Exit(1)
	}

	be, err := newBackendFromConfig(cfg.Backend)
	if err != nil {
		logger.Error("backend construction failed", "err", err)
		os.Exit(1)
	}

	inferenceSvc := services.NewInferenceServer(be)
	healthSvc := services.NewHealthServer(be)

	mux := buildMux(inferenceSvc, healthSvc, logger)
	handler := wrapMiddleware(mux)

	// h2c wraps the handler so cleartext HTTP/2 (gRPC over plain TCP)
	// works alongside HTTP/1.1. Connect clients negotiate HTTP/1.1 +
	// JSON or HTTP/2 + protobuf depending on configuration; we accept
	// both on the same port.
	srv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      h2c.NewHandler(handler, &http2.Server{}),
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSec) * time.Second,
	}

	logger.Info("control plane listening", "addr", cfg.Server.Addr,
		"backend.engine", cfg.Backend.Engine,
		"backend.url", cfg.Backend.URL,
		"deployment.provider", cfg.Deployment.Provider)

	err = skhttp.ListenAndServeGraceful(srv,
		skhttp.WithDrainTimeout(time.Duration(cfg.Server.ShutdownSec)*time.Second),
		skhttp.WithOnShutdown(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdownTel(ctx); err != nil {
				logger.Error("telemetry shutdown failed", "err", err)
			}
		}),
	)
	if err != nil {
		logger.Error("server exited with error", "err", err)
		os.Exit(1)
	}
	logger.Info("control plane stopped")
}

// buildMux composes the routing tree. Connect-rpc handlers register
// themselves at fixed paths from the proto package + service name;
// the OpenAI HTTP handler registers /v1/completions, /v1/chat/completions,
// and /health on the same mux.
func buildMux(inference *services.InferenceServer, health *services.HealthServer, logger *slog.Logger) *http.ServeMux {
	mux := http.NewServeMux()

	// Connect-RPC: typed/SDK clients hit the generated paths.
	inferencePath, inferenceConnectHandler := inferenceplanev1connect.NewInferenceServiceHandler(inference)
	healthPath, healthConnectHandler := inferenceplanev1connect.NewHealthServiceHandler(health)
	mux.Handle(inferencePath, inferenceConnectHandler)
	mux.Handle(healthPath, healthConnectHandler)

	// OpenAI compat: existing OpenAI SDK clients hit these.
	openai.New(inference, health, logger).Register(mux)

	return mux
}

// wrapMiddleware wraps the mux in the request-handling chain. Outer
// to inner: otelhttp -> RequestID -> Recovery -> RequestLogger.
//
// otelhttp is outermost so the auto-generated server span wraps every
// other layer, including request-ID generation -- so the request ID
// and the trace ID are correlated. Logging skips /health so health
// probes don't drown out real traffic at production rates.
func wrapMiddleware(mux *http.ServeMux) http.Handler {
	return otelhttp.NewHandler(
		skmw.NewRequestID().Middleware(
			skmw.Recovery(
				skmw.RequestLogger("/health")(mux),
			),
		),
		"controlplane",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}

// newBackendFromConfig constructs the configured Backend. v0.1 only
// recognizes "vllm"; later versions extend this with "sglang",
// "tensorrt", "llamacpp" -- each behind the same Backend interface.
func newBackendFromConfig(cfg config.BackendConfig) (backends.Backend, error) {
	switch cfg.Engine {
	case "vllm":
		return backends.NewVLLM(cfg.Name, cfg.URL), nil
	case "":
		return nil, errors.New("controlplane: backend.engine is required (got empty string)")
	default:
		return nil, errors.New("controlplane: unsupported backend.engine: " + cfg.Engine)
	}
}
