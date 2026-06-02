package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"

	skhttp "github.com/panyam/servicekit/http"
	skmw "github.com/panyam/servicekit/middleware"

	inferencev1 "github.com/inference-book/inference-plane/gen/go/inferenceplane/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/backends"
	"github.com/inference-book/inference-plane/internal/config"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/lifecycle"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/router"
	"github.com/inference-book/inference-plane/internal/services"
	"github.com/inference-book/inference-plane/internal/telemetry"
	"github.com/inference-book/inference-plane/internal/web/server"
)

// grpcAddr is the localhost-only address the gRPC server listens on.
// In-process implementation detail, not a public surface.
const grpcAddr = "127.0.0.1:9090"

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the control plane server",
	Long: `Boots the v0.1 control plane: gRPC server on 127.0.0.1:9090
(loopback, source of truth) plus HTTP server on the configured public
port (default :8080) hosting both the OpenAI-compatible REST surface
(grpc-gateway) and the typed Connect-RPC handlers.

Configuration sources, in increasing precedence:

  1. Built-in defaults
  2. YAML file (--config / deploy/config.yaml / /etc/iplane/config.yaml)
  3. Environment (IPLANE_*, e.g. IPLANE_SERVER_ADDR=:9000)
  4. Flags (--server-addr, --backend-engine, etc.)`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServe(cmd.Context())
	},
}

func init() {
	rootCmd.AddCommand(serveCmd)
	bindServeFlags(serveCmd)
	registerServeDefaults()
}

// bindServeFlags declares the most-frequently-tweaked config fields as
// flags and binds them to dotted viper keys. Flags use kebab-case;
// viper keys are dotted to match the YAML structure; env vars are
// IPLANE_<UPPER_SNAKE> via the prefix + replacer set in initConfig.
func bindServeFlags(c *cobra.Command) {
	c.Flags().String("server-addr", ":8080", "HTTP server bind address")
	c.Flags().String("state-dir", "", "directory holding state.json + .lock (default ~/.iplane; IPLANE_STATE_DIR env also honored)")
	c.Flags().String("backend-engine", "mock", "backend engine (mock | vllm)")
	c.Flags().String("backend-url", "", "backend base URL (ignored by mock)")
	c.Flags().String("backend-name", "mock", "backend name label for metrics/logs")
	c.Flags().String("otlp-endpoint", "localhost:4317", "OpenTelemetry collector endpoint")
	c.Flags().String("service-name", "inference-plane", "OTel service name")
	c.Flags().String("environment", "dev", "OTel deployment.environment")
	c.Flags().String("provider", "", "deployment provider label")
	c.Flags().String("gpu-type", "", "deployment gpu_type label")
	c.Flags().String("billing-mode", "", "deployment billing_mode label")
	c.Flags().String("instance-id", "", "deployment instance_id label")

	// Bind kebab-case flags onto dotted viper keys matching the YAML.
	for flagName, key := range map[string]string{
		"server-addr":     "server.addr",
		"state-dir":       "state.dir",
		"backend-engine":  "backend.engine",
		"backend-url":     "backend.url",
		"backend-name":    "backend.name",
		"otlp-endpoint":   "telemetry.otlp_endpoint",
		"service-name":    "telemetry.service_name",
		"environment":     "telemetry.environment",
		"provider":        "deployment.provider",
		"gpu-type":        "deployment.gpu_type",
		"billing-mode":    "deployment.billing_mode",
		"instance-id":     "deployment.instance_id",
	} {
		_ = viper.BindPFlag(key, c.Flags().Lookup(flagName))
	}
}

// registerServeDefaults sets the bottom of the precedence stack.
// Anything a flag, env, or YAML doesn't supply falls back to these.
func registerServeDefaults() {
	viper.SetDefault("server.addr", ":8080")
	viper.SetDefault("server.read_timeout_sec", 60)
	viper.SetDefault("server.write_timeout_sec", 600) // long enough for slow generations
	viper.SetDefault("server.shutdown_sec", 30)

	viper.SetDefault("backend.engine", "mock")
	viper.SetDefault("backend.url", "")
	viper.SetDefault("backend.name", "mock")

	viper.SetDefault("telemetry.otlp_endpoint", "localhost:4317")
	viper.SetDefault("telemetry.service_name", "inference-plane")
	viper.SetDefault("telemetry.environment", "dev")
	viper.SetDefault("telemetry.sample_ratio", 1.0)

	// router.queue: 0 servicers = Beat 1 behavior (no queue). Capacity
	// has a default but only kicks in when servicers > 0.
	viper.SetDefault("router.queue.servicers", 0)
	viper.SetDefault("router.queue.capacity", 256)
}

// loopbackURL turns the daemon's HTTP bind address into a fully-qualified
// URL the in-daemon router can dial. Forms:
//
//	":8080"           -> "http://localhost:8080"
//	"0.0.0.0:8080"    -> "http://localhost:8080" (rewrite to loopback)
//	"127.0.0.1:8080"  -> "http://127.0.0.1:8080"
//	"host:8080"       -> "http://host:8080"
//
// The loopback rewrite for 0.0.0.0 matters because the router's
// Connect client needs a routable address; a literal 0.0.0.0 client
// dial would fail on most platforms.
func loopbackURL(addr string) string {
	if len(addr) > 0 && addr[0] == ':' {
		return "http://localhost" + addr
	}
	if len(addr) >= 8 && addr[:8] == "0.0.0.0:" {
		return "http://localhost:" + addr[8:]
	}
	return "http://" + addr
}

// resolveServeStateDir picks the directory holding state.json + the
// flock. Precedence: --state-dir flag (via viper "state.dir"), then
// IPLANE_STATE_DIR env, then ~/.iplane. Matches the one-shot CLI's
// resolveStateDir so the daemon and CLI agree on the canonical path
// without coordination.
func resolveServeStateDir() (string, error) {
	if dir := viper.GetString("state.dir"); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("IPLANE_STATE_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	return filepath.Join(home, ".iplane"), nil
}

// loadConfig assembles a *config.Config from viper's resolved view.
// Uses TagName="yaml" so the existing yaml struct tags drive the
// mapping -- avoids dual-tagging every field with both `yaml` and
// `mapstructure`.
func loadConfig() (*config.Config, error) {
	var cfg config.Config
	useYAMLTags := func(c *mapstructure.DecoderConfig) { c.TagName = "yaml" }
	if err := viper.Unmarshal(&cfg, useYAMLTags); err != nil {
		return nil, fmt.Errorf("config unmarshal: %w", err)
	}
	if err := config.Validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func runServe(parent context.Context) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		return fmt.Errorf("config load: %w", err)
	}

	shutdownTel, err := telemetry.Init(parent, cfg.Telemetry, cfg.Deployment)
	if err != nil {
		return fmt.Errorf("telemetry init: %w", err)
	}

	be, err := newBackendFromConfig(cfg.Backend)
	if err != nil {
		return fmt.Errorf("backend: %w", err)
	}

	recorder, err := metrics.NewRecorder()
	if err != nil {
		return fmt.Errorf("metrics: %w", err)
	}

	costProviders, err := metrics.LoadProviders("providers.yaml")
	if err != nil {
		// Non-fatal: a deployment without providers.yaml emits the
		// uptime + active counters but skips the cross-provider snapshot.
		logger.Warn("providers.yaml not loaded; cross-provider snapshot disabled", "err", err)
		costProviders = nil
	}
	costRecorder, err := metrics.NewCostRecorder(metrics.Deployment{
		Provider:    cfg.Deployment.Provider,
		GPUType:     cfg.Deployment.GPUType,
		BillingMode: cfg.Deployment.BillingMode,
		InstanceID:  cfg.Deployment.InstanceID,
	}, costProviders)
	if err != nil {
		return fmt.Errorf("cost recorder: %w", err)
	}

	// Daemon state-of-record. Open the state store, acquire the
	// lifetime flock (non-blocking -- fail-fast if another daemon
	// already holds it), and build the provisioners Service. The
	// release func runs on graceful shutdown; deferred so even a
	// startup-mid-failure tears down the lock cleanly.
	stateDir, err := resolveServeStateDir()
	if err != nil {
		return fmt.Errorf("resolve state dir: %w", err)
	}
	stateStore, err := file.Open(stateDir, "default")
	if err != nil {
		return fmt.Errorf("open state store at %q: %w", stateDir, err)
	}
	releaseLock, err := stateStore.LockForLifetime()
	if err != nil {
		var held *file.ErrLockHeld
		if errors.As(err, &held) {
			if held.HolderPID != 0 {
				return fmt.Errorf("another iplane serve is already running at PID %d (state %s); only one daemon per state dir", held.HolderPID, held.Path)
			}
			return fmt.Errorf("state directory %q is locked by another process; only one daemon per state dir", held.Path)
		}
		return fmt.Errorf("acquire state lock: %w", err)
	}
	defer releaseLock()

	provisionerSvc, err := buildLocalService(stateStore, "default")
	if err != nil {
		return fmt.Errorf("build provisioner service: %w", err)
	}
	logger.Info("daemon state-of-record initialized", "state_dir", stateDir)

	// v0.2 ch7-beat1.7: launch the idle-TTL reaper goroutine. Sweeps
	// every 30s, destroys deployments whose idle TTL has elapsed
	// (state==RUNNING && idle_ttl_seconds > 0 && !no_idle_destroy).
	// Default TTL is 0 (no reap) so v0.1 deployments are unaffected;
	// operators opt in via `--idle-ttl` on deploy or `iplane up`.
	//
	// Lifecycle: ctx-cancelled on daemon shutdown so the goroutine
	// exits cleanly before telemetry shutdown flushes spans.
	reaperCtx, reaperCancel := context.WithCancel(parent)
	defer reaperCancel()
	reaper := lifecycle.New(provisionerSvc, lifecycle.WithRecorder(recorder), lifecycle.WithLogger(logger))
	go reaper.Run(reaperCtx)
	logger.Info("idle-TTL reaper started", "interval", lifecycle.DefaultInterval)

	grpcSrv, grpcLis, err := startGRPCServer(be, recorder, costRecorder, logger)
	if err != nil {
		return fmt.Errorf("gRPC server: %w", err)
	}
	defer grpcLis.Close()

	// Construct the v0.2 data-plane router. Per CONSTRAINTS.md's
	// CP/DP-1, the router reaches deployment state only through the
	// generated DeploymentService Connect client; in `iplane serve`
	// that client loopback-dials this same HTTP listener.
	//
	// router.queue.servicers > 0 activates the v0.2 Beat 2 M/M/k
	// waiting room; 0 (the default in deploy/config.yaml) keeps
	// Beat 1's direct-forward path.
	daemonBaseURL := loopbackURL(cfg.Server.Addr)
	deploymentRouter := router.New(
		provisionerv1connect.NewDeploymentServiceClient(http.DefaultClient, daemonBaseURL),
		recorder,
		router.WithQueue(cfg.Router.Queue.Servicers, cfg.Router.Queue.Capacity),
	)
	deploymentRouter.Start(parent)

	api, err := server.New(parent, grpcAddr, logger,
		server.WithProvisionerHandler(provisioners.NewConnectProvisionerAdapter(provisionerSvc)),
		server.WithDeploymentHandler(provisioners.NewConnectDeploymentAdapter(provisionerSvc)),
		server.WithDataPlaneRoutes(deploymentRouter.Handle()),
	)
	if err != nil {
		return fmt.Errorf("HTTP API: %w", err)
	}

	httpSrv := &http.Server{
		Addr:         cfg.Server.Addr,
		Handler:      h2c.NewHandler(wrapServeMiddleware(api.Handler()), &http2.Server{}),
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
			// Drain the router queue before tearing down gRPC and
			// telemetry: in-flight engine calls keep firing until the
			// pool's servicers exit, and their spans / metrics need
			// the telemetry SDK alive to flush.
			deploymentRouter.Shutdown()
			grpcSrv.GracefulStop()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdownTel(ctx); err != nil {
				logger.Error("telemetry shutdown failed", "err", err)
			}
		}),
	)
	if err != nil {
		return fmt.Errorf("server exited: %w", err)
	}
	logger.Info("control plane stopped")
	return nil
}

// startGRPCServer registers the gRPC handlers on a localhost-only
// listener and serves in a goroutine. The HTTP layer in
// internal/web/server dials this listener for both gateway and
// connect handlers.
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

// wrapServeMiddleware composes the HTTP middleware chain. Outer to
// inner: otelhttp -> RequestID -> Recovery -> RequestLogger.
func wrapServeMiddleware(h http.Handler) http.Handler {
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
func newBackendFromConfig(cfg config.BackendConfig) (backends.Backend, error) {
	switch cfg.Engine {
	case "vllm":
		return backends.NewVLLM(cfg.Name, cfg.URL), nil
	case "mock":
		return backends.NewMock(cfg.Name), nil
	case "":
		return nil, errors.New("backend.engine is required (got empty string)")
	default:
		return nil, errors.New("unsupported backend.engine: " + cfg.Engine)
	}
}
