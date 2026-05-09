// Command controlplane is the v0.1 entrypoint.
//
// Topology: one process, two listeners.
//
//   - gRPC server on a localhost-only port (default 127.0.0.1:9090).
//     Hosts InferenceService and HealthService. Source of truth for
//     the API; the HTTP layer below dials it for both gateway and
//     connect surfaces. Localhost-only because the gRPC port isn't
//     meant to be exposed externally -- HTTP at :8080 is the public
//     surface.
//
//   - HTTP server on the configured public port (default :8080). Mux
//     hosts grpc-gateway routes (OpenAI-shaped JSON: /v1/completions,
//     /v1/chat/completions, /health) and Connect-RPC handlers (typed
//     SDK clients hit /inferenceplane.v1.* paths).
//
// Lifecycle: load config, init telemetry, build backend and services,
// start gRPC server in a goroutine, build HTTP API, run the HTTP
// server via servicekit's graceful runner. On shutdown the graceful
// runner drains HTTP requests; an OnShutdown callback flushes
// telemetry and stops the gRPC server.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"

	skhttp "github.com/panyam/servicekit/http"
	skmw "github.com/panyam/servicekit/middleware"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
	"github.com/inference-book/inference-plane/internal/backends"
	"github.com/inference-book/inference-plane/internal/config"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/services"
	"github.com/inference-book/inference-plane/internal/telemetry"
	"github.com/inference-book/inference-plane/internal/web/server"
)

// grpcAddr is the localhost-only address the gRPC server listens on.
// Not configurable for v0.1 -- it's an in-process implementation
// detail, not a public surface. Wire it up to config when v0.2 lets
// operators co-locate or split the gRPC and HTTP listeners.
const grpcAddr = "127.0.0.1:9090"

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

	// Application metrics (the four families from chapter 6.6.2).
	recorder, err := metrics.NewRecorder()
	if err != nil {
		logger.Error("metrics recorder init failed", "err", err)
		os.Exit(1)
	}

	// Cost metrics: load providers.yaml to populate per-provider rate
	// gauges; the deployment-identity labels come from config.
	providers, err := metrics.LoadProviders("providers.yaml")
	if err != nil {
		// Not fatal -- a deployment without providers.yaml emits the
		// uptime + active counters but skips the cross-provider snapshot.
		logger.Warn("providers.yaml not loaded; cross-provider snapshot disabled", "err", err)
		providers = nil
	}
	costRecorder, err := metrics.NewCostRecorder(metrics.Deployment{
		Provider:    cfg.Deployment.Provider,
		GPUType:     cfg.Deployment.GPUType,
		BillingMode: cfg.Deployment.BillingMode,
		InstanceID:  cfg.Deployment.InstanceID,
	}, providers)
	if err != nil {
		logger.Error("cost recorder init failed", "err", err)
		os.Exit(1)
	}

	// gRPC server: source of truth for the API.
	grpcSrv, grpcLis, err := startGRPCServer(be, recorder, costRecorder, logger)
	if err != nil {
		logger.Error("gRPC server start failed", "err", err)
		os.Exit(1)
	}
	defer grpcLis.Close()

	// HTTP API: gateway routes + connect handlers, both dialing the
	// gRPC server via the local loopback address.
	api, err := server.New(context.Background(), grpcAddr, logger)
	if err != nil {
		logger.Error("HTTP API build failed", "err", err)
		os.Exit(1)
	}

	httpSrv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      h2c.NewHandler(wrapMiddleware(api.Handler()), &http2.Server{}),
		ReadTimeout:  time.Duration(cfg.Server.ReadTimeoutSec) * time.Second,
		WriteTimeout: time.Duration(cfg.Server.WriteTimeoutSec) * time.Second,
	}

	logger.Info("control plane listening",
		"http", cfg.Server.Addr,
		"grpc", grpcAddr,
		"backend.engine", cfg.Backend.Engine,
		"backend.url", cfg.Backend.URL,
		"deployment.provider", cfg.Deployment.Provider)

	err = skhttp.ListenAndServeGraceful(httpSrv,
		skhttp.WithDrainTimeout(time.Duration(cfg.Server.ShutdownSec)*time.Second),
		skhttp.WithOnShutdown(func() {
			grpcSrv.GracefulStop()
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

// startGRPCServer constructs the gRPC server, registers the services,
// starts the listener on grpcAddr, and serves in a goroutine. Returns
// the server (so main can GracefulStop it) and the listener (so main
// can close it on cleanup).
func startGRPCServer(be backends.Backend, rec *metrics.Recorder, cost *metrics.CostRecorder, logger *slog.Logger) (*grpc.Server, net.Listener, error) {
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return nil, nil, err
	}

	srv := grpc.NewServer()
	inferencev1.RegisterInferenceServiceServer(srv, services.NewInferenceServer(be, rec, cost))
	inferencev1.RegisterHealthServiceServer(srv, services.NewHealthServer(be, rec))

	go func() {
		if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			logger.Error("gRPC server crashed", "err", err)
		}
	}()
	return srv, lis, nil
}

// wrapMiddleware wraps the API handler in the request-handling chain.
// Outer to inner: otelhttp -> RequestID -> Recovery -> RequestLogger.
//
// otelhttp is outermost so the auto-generated server span wraps every
// other layer, including request-ID generation -- request ID and
// trace ID are correlated. Logging skips /health so health probes
// don't drown out real traffic at production rates.
func wrapMiddleware(h http.Handler) http.Handler {
	return otelhttp.NewHandler(
		skmw.NewRequestID().Middleware(
			skmw.Recovery(
				skmw.RequestLogger("/health")(h),
			),
		),
		"controlplane",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)
}

// newBackendFromConfig constructs the configured Backend.
//
//	"vllm" : real vLLM container; requires NVIDIA GPU and the docker
//	         compose --profile gpu flag to pull in the vllm service.
//	"mock" : synthetic responses with configurable latency and token
//	         counts. The default for local development on hosts
//	         without a GPU; also the right choice for CI smoke tests
//	         and dashboard authoring with synthetic traffic.
//
// Later versions add "sglang", "tensorrt", "llamacpp" -- each behind
// the same Backend interface.
func newBackendFromConfig(cfg config.BackendConfig) (backends.Backend, error) {
	switch cfg.Engine {
	case "vllm":
		return backends.NewVLLM(cfg.Name, cfg.URL), nil
	case "mock":
		return backends.NewMock(cfg.Name), nil
	case "":
		return nil, errors.New("controlplane: backend.engine is required (got empty string)")
	default:
		return nil, errors.New("controlplane: unsupported backend.engine: " + cfg.Engine)
	}
}
