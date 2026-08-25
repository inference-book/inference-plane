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
	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/backends"
	"github.com/inference-book/inference-plane/internal/config"
	"github.com/inference-book/inference-plane/internal/engines"
	"github.com/inference-book/inference-plane/internal/healthcheck"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/lifecycle"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/router"
	"github.com/inference-book/inference-plane/internal/router/policy"
	"github.com/inference-book/inference-plane/internal/scheduler"
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

// routingPolicyLabel normalizes an empty routing-policy value to
// "round_robin" for the startup log, so an unset config reads as its
// effective default rather than a blank field.
func routingPolicyLabel(p string) string {
	if p == "" {
		return "round_robin"
	}
	return p
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
	c.Flags().String("routing-policy", "round_robin", "per-request replica selection: round_robin | prefix_affinity")

	// Bind kebab-case flags onto dotted viper keys matching the YAML.
	for flagName, key := range map[string]string{
		"server-addr":    "server.addr",
		"state-dir":      "state.dir",
		"backend-engine": "backend.engine",
		"backend-url":    "backend.url",
		"backend-name":   "backend.name",
		"otlp-endpoint":  "telemetry.otlp_endpoint",
		"service-name":   "telemetry.service_name",
		"environment":    "telemetry.environment",
		"routing-policy": "router.routing_policy",
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

	// 127.0.0.1 (not localhost): the compose otel-collector binds
	// 127.0.0.1:4317 IPv4-only, and gRPC's name resolver on macOS
	// Docker returns "produced zero addresses" for the "localhost"
	// hostname. Override via IPLANE_TELEMETRY_OTLP_ENDPOINT in compose
	// (e.g. "otel-collector:4317" for the container-network path).
	viper.SetDefault("telemetry.otlp_endpoint", "127.0.0.1:4317")
	viper.SetDefault("router.touch_debounce_interval", provisioners.DefaultTouchDebounceInterval)
	viper.SetDefault("telemetry.service_name", "inference-plane")
	viper.SetDefault("telemetry.environment", "dev")
	viper.SetDefault("telemetry.sample_ratio", 1.0)

	// router.queue: 0 servicers = Beat 1 behavior (no queue). Capacity
	// has a default but only kicks in when servicers > 0. Beat 2.3
	// adds per-lane sub-blocks; default both to 0 so behavior is
	// "use the top-level setting if any, otherwise no queue."
	viper.SetDefault("router.queue.servicers", 0)
	viper.SetDefault("router.queue.capacity", 256)
	viper.SetDefault("router.queue.in_flight_cap", 0)
	viper.SetDefault("router.queue.interactive.servicers", 0)
	viper.SetDefault("router.queue.interactive.capacity", 256)
	viper.SetDefault("router.queue.batch.servicers", 0)
	viper.SetDefault("router.queue.batch.capacity", 256)
	viper.SetDefault("router.routing_policy", "round_robin")
	viper.SetDefault("router.affinity_overload_threshold", 0)

	// health: per-replica health-poll loop (#87). Defaults match
	// healthcheck.DefaultConfig() so operators who omit the block
	// see the same behavior the package documents.
	viper.SetDefault("health.poll_interval", 10*time.Second)
	viper.SetDefault("health.failure_threshold", 3)
	viper.SetDefault("health.success_threshold", 3)
	viper.SetDefault("health.probe_timeout", 2*time.Second)
	viper.SetDefault("health.max_concurrent", 32)
	viper.SetDefault("health.activity_window", 2*time.Minute)
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

	shutdownTel, err := telemetry.Init(parent, cfg.Telemetry)
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

	// Two cases distinguished here:
	//
	//   - File missing entirely: legitimate deployment shape (fresh
	//     checkout, mock-only dev, custom catalog elsewhere). Warn and
	//     proceed without the cross-provider snapshot panel.
	//
	//   - File present but unreadable / malformed: operator
	//     misconfiguration. Fail fast so the broken YAML surfaces at
	//     startup, not as a silently empty dashboard panel hours later.
	var costProviders []metrics.Provider
	if _, statErr := os.Stat("providers.yaml"); errors.Is(statErr, os.ErrNotExist) {
		logger.Warn("providers.yaml not found; cross-provider snapshot disabled")
	} else {
		provs, err := metrics.LoadProviders("providers.yaml")
		if err != nil {
			return fmt.Errorf("providers.yaml: %w", err)
		}
		costProviders = provs
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

	// Built before the Service, which is the reverse of the dependency
	// everything else here has, because the Service takes a reader over it
	// as a construction option. The registry needs only the state store, so
	// it can be built this early; the pieces that genuinely need the Service
	// (the sweeper's drainer, the locator) are wired further down once both
	// exist.
	engineRegistry := engines.New(engines.NewStateStore(stateStore))

	provisionerSvc, err := buildLocalService(stateStore, "default",
		provisioners.WithTouchDebounceInterval(cfg.Router.TouchDebounceInterval),
		provisioners.WithRecorder(recorder),
		// Overrides buildLocalService's default HF store with the
		// warm-cache wrap when model_cache is set (no-op otherwise).
		provisioners.WithModelStore(modelStoreFromConfig(cfg.ModelCache)),
		// Lets the deploy path tell a weight download from a load during
		// engine:init. internal/engines imports internal/provisioners, so
		// the dependency can only run this way: a reader injected here
		// rather than provisioners reaching for the registry itself. The
		// mirror of engines.WithDrainer(provisionerSvc) below.
		provisioners.WithStagingReader(func(_ context.Context, engineID string) (*provisionerv1.StagingProgress, bool) {
			e, err := engineRegistry.Get(engineID)
			if err != nil || e == nil {
				return nil, false
			}
			return e.GetStaging(), e.GetStaging() != nil
		}),
	)
	if err != nil {
		return fmt.Errorf("build provisioner service: %w", err)
	}
	logger.Info("daemon state-of-record initialized", "state_dir", stateDir)

	// Built after the Service because the Service is its fleet source.
	// Cost is measured from the instances iplane rented, so the recorder
	// needs something that can enumerate them; it used to be handed a
	// tuple the operator asserted in their shell (#163).
	costRecorder, err := metrics.NewCostRecorder(provisionerSvc, costProviders)
	if err != nil {
		return fmt.Errorf("cost recorder: %w", err)
	}

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

	// v0.2 ch7-beat3.5 (#87): launch the per-replica health-poll
	// goroutine. Probes each replica's <engine_endpoint>/health on a
	// tick; K-of-K consecutive failures push the replica into the
	// deployment's unhealthy_instance_ids set (where the router
	// skips it). K-of-K consecutive passes on a quarantined replica
	// restore it. Defaults yield ~30s to first quarantine on a hung
	// replica.
	//
	// Lifecycle: ctx-cancelled on daemon shutdown so the goroutine
	// exits cleanly before telemetry flush.
	healthCtx, healthCancel := context.WithCancel(parent)
	defer healthCancel()
	healthCfg := healthcheck.Config{
		PollInterval:     viper.GetDuration("health.poll_interval"),
		FailureThreshold: viper.GetInt("health.failure_threshold"),
		SuccessThreshold: viper.GetInt("health.success_threshold"),
		ProbeTimeout:     viper.GetDuration("health.probe_timeout"),
		MaxConcurrent:    viper.GetInt("health.max_concurrent"),
		ActivityWindow:   viper.GetDuration("health.activity_window"),
	}
	healthAdapter := healthcheck.NewServiceAdapter(provisionerSvc)
	healthRunner := healthcheck.New(healthCfg, healthAdapter, healthAdapter, logger)
	// Started below, once the router exists: the runner takes the router as
	// its activity reporter, and attaching that after Run has begun would
	// race the tick goroutine.

	// v0.2 ch10 (#204): the push side of fleet tracking. Engines register
	// themselves and renew a lease; the sweeper declares an engine LOST when
	// it stops renewing.
	//
	// This runs ALONGSIDE the health poller above, not instead of it. The
	// poller drives quarantine, and quarantine is router eligibility, which
	// the data path's correctness rides on; the registry carries membership,
	// group composition and liveness, which no probe can report. Removing
	// either leaves a hole the other does not cover.
	engineSweeper := engines.NewSweeper(engineRegistry, engines.WithLogger(logger))
	go engineSweeper.Run(healthCtx)
	logger.Info("engine registry started",
		"lease", engines.DefaultLease,
		"renew_interval", engineRegistry.RenewInterval())

	grpcSrv, grpcLis, err := startGRPCServer(be, recorder, logger)
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
	// waiting room. Beat 2.3 added per-lane sub-blocks so operators
	// can tune interactive and batch independently; if those are
	// unset, the top-level (servicers, capacity) applies to both
	// lanes (Beat 2.1 backward-compat).
	daemonBaseURL := loopbackURL(cfg.Server.Addr)
	routerOpts := []router.Option{
		router.WithQueue(cfg.Router.Queue.Servicers, cfg.Router.Queue.Capacity),
		router.WithInteractiveQueue(cfg.Router.Queue.Interactive.Servicers, cfg.Router.Queue.Interactive.Capacity),
		router.WithBatchQueue(cfg.Router.Queue.Batch.Servicers, cfg.Router.Queue.Batch.Capacity),
		router.WithInFlightCap(cfg.Router.Queue.InFlightCap),
		router.WithTenantWeights(scheduler.Weights(cfg.Router.Queue.TenantWeights)),
	}
	// v0.2 ch8: per-request replica-selection policy. Default (or empty)
	// is round-robin, which router.New already installs. prefix_affinity
	// installs the sticky-routing policy. Unknown values fail fast so a
	// typo doesn't silently fall back to round-robin.
	switch cfg.Router.RoutingPolicy {
	case "", "round_robin":
		// router.New's default (RoundRobin); no option needed.
	case "prefix_affinity":
		routerOpts = append(routerOpts, router.WithRoutingPolicy(policy.NewPrefixAffinity(cfg.Router.AffinityOverloadThreshold)))
	default:
		return fmt.Errorf("router.routing_policy: unknown value %q (want round_robin | prefix_affinity)", cfg.Router.RoutingPolicy)
	}
	routerOpts = append(routerOpts, router.WithCostRecorder(costRecorder))
	deploymentRouter := router.New(
		provisionerv1connect.NewDeploymentServiceClient(http.DefaultClient, daemonBaseURL),
		recorder,
		routerOpts...,
	)
	deploymentRouter.Start(parent)

	// The router is the only component that knows whether an engine is
	// still turning requests around, so the health runner starts here
	// rather than at construction. Without it a saturated engine that
	// cannot spare a 2s /health response inside three ticks gets
	// quarantined, and every request then 503s against a deployment that
	// was working (#450).
	healthRunner.WithActivity(deploymentRouter)
	go healthRunner.Run(healthCtx)
	logger.Info("health-poll runner started",
		"poll_interval", healthCfg.PollInterval,
		"failure_threshold", healthCfg.FailureThreshold,
		"success_threshold", healthCfg.SuccessThreshold,
		"activity_window", healthCfg.ActivityWindow)

	// Echo the scheduler / queue config at startup so operators can
	// confirm which values the daemon actually loaded. Otherwise it's
	// guesswork whether per-demo config.yaml overrides took effect.
	logger.Info("router queue config loaded",
		"servicers", cfg.Router.Queue.Servicers,
		"capacity", cfg.Router.Queue.Capacity,
		"in_flight_cap", cfg.Router.Queue.InFlightCap,
		"interactive_servicers", cfg.Router.Queue.Interactive.Servicers,
		"interactive_capacity", cfg.Router.Queue.Interactive.Capacity,
		"batch_servicers", cfg.Router.Queue.Batch.Servicers,
		"batch_capacity", cfg.Router.Queue.Batch.Capacity,
		"touch_debounce_interval", cfg.Router.TouchDebounceInterval,
		"routing_policy", routingPolicyLabel(cfg.Router.RoutingPolicy))

	api, err := server.New(parent, grpcAddr, logger,
		server.WithProvisionerHandler(provisioners.NewConnectProvisionerAdapter(provisionerSvc)),
		server.WithDeploymentHandler(provisioners.NewConnectDeploymentAdapter(provisionerSvc)),
		server.WithDataPlaneRoutes(deploymentRouter.Handle()),
		server.WithEngineRegistryHandler(engines.NewConnectAdapter(engineRegistry,
			engines.WithDrainer(provisionerSvc),
			// The two identity fields a container cannot know about itself:
			// its provider machine id and its externally reachable endpoint.
			// The agent is told a correlation key at deploy time and the
			// control plane completes the record from what it already
			// recorded when it rented the box.
			engines.WithLocator(engines.LocatorFunc(
				func(_ context.Context, engineID string) (engines.NodeIdentity, bool, error) {
					host, provider, endpoint, found, err := provisionerSvc.LocateEngineNode(engineID)
					return engines.NodeIdentity{
						HostID:   host,
						Provider: provider,
						Endpoint: endpoint,
					}, found, err
				})),
			// A drain is a synchronous unary call, so its wait cannot exceed
			// the response write deadline. Deriving the cap from the same
			// config value keeps the two from drifting; Ch 9's `unexpected
			// EOF` was exactly this pair disagreeing. Leave headroom for the
			// teardown that follows the wait.
			engines.WithMaxDrainTimeout(
				time.Duration(cfg.Server.WriteTimeoutSec)*time.Second/2),
		)),
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
	)

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
func startGRPCServer(be backends.Backend, rec *metrics.Recorder, logger *slog.Logger) (*grpc.Server, net.Listener, error) {
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		return nil, nil, err
	}
	srv := grpc.NewServer()
	inferencev1.RegisterInferenceServiceServer(srv, services.NewInferenceServer(be, rec))
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
