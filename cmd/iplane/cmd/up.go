package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/chzyer/readline"
	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
	"github.com/inference-book/inference-plane/gen/go/provisioner/v1/provisionerv1connect"
	"github.com/inference-book/inference-plane/internal/config"
	"github.com/inference-book/inference-plane/internal/deployments/sshdocker"
	"github.com/inference-book/inference-plane/internal/metrics"
	"github.com/inference-book/inference-plane/internal/provisioners"
	"github.com/inference-book/inference-plane/internal/provisioners/local"
	"github.com/inference-book/inference-plane/internal/provisioners/runpod"
	"github.com/inference-book/inference-plane/internal/provisioners/stores/file"
	"github.com/inference-book/inference-plane/internal/router"
	"github.com/inference-book/inference-plane/internal/sshkeys"
	"github.com/inference-book/inference-plane/internal/telemetry"
)

// `iplane up` — the chapter's flagship one-liner. Provisions a GPU,
// runs the engine image as a pod, drops into a chat REPL, and tears
// everything down on exit. Single instance, single model: replica /
// multi-model variants are v0.2+ (see ROADMAP).
//
// The verb is purely orchestration over primitives that already exist:
//
//	CreateDeployment{auto-provision, wait=true}  // Phase 1+2
//	WatchDeployment                              // progress stream
//	POST /v1/chat/completions                    // direct dial of proxy URL
//	DestroyDeployment                            // cleanup on exit
//
// Telemetry plumbing (OTEL_EXPORTER_OTLP_*) follows the same shape as
// `iplane deployment deploy` -- env fallbacks + flags + an explicit
// `--no-telemetry` escape hatch.
var (
	upProvider     string
	upModel        string
	upClass        string
	upImage        string
	upRegion       string
	upOtelEndpoint string
	upOtelHeaders  map[string]string
	upNoTelemetry  bool
	upID           string
	upTimeout      time.Duration
	upNoChat       bool
	upDebugShell    bool
	upIdleTTL       time.Duration
	upNoIdleDestroy bool
	upMaxTokens     int32
	upTemperature  float64
)

const upDefaultImage = "vllm/vllm-openai:v0.7.0"

var upCmd = &cobra.Command{
	Use:   "up",
	Short: "Provision + deploy + chat in one command (the iplane flagship verb)",
	Args:  cobra.NoArgs,
	Long: `Stand up a model on a GPU pod and start chatting with it.

What this does:

  1. Auto-provisions a GPU instance (small class by default).
  2. Runs the engine image as that pod (image-as-pod).
  3. Waits for the engine's /health to return 2xx.
  4. Drops you into a chat REPL: type a prompt, see the response.
  5. On exit (empty prompt OR Ctrl-C), terminates the pod.

Telemetry: if IPLANE_OTEL_ENDPOINT (or --otel-endpoint) is set, the
engine ships traces to that sink. Use --no-telemetry to skip silently
when you don't want telemetry at all.

This is the v0.1 single-instance flow. Multi-replica / load-balanced
variants land in v0.2 (chapter 7 brings continuous batching + a
queue, which is the right time to add a router).`,
	Example: `  # Simplest invocation (assumes RUNPOD_API_KEY in env, OTel optional)
  iplane up --model Qwen/Qwen2.5-1.5B-Instruct

  # With a hosted OTel sink
  export IPLANE_OTEL_ENDPOINT=https://otlp-gateway-prod-XXX.grafana.net/otlp
  export IPLANE_OTEL_HEADERS='Authorization=Basic <token>'
  iplane up --model Qwen/Qwen2.5-1.5B-Instruct

  # Skip the REPL, just print the endpoint and block on Ctrl-C
  iplane up --model Qwen/Qwen2.5-1.5B-Instruct --no-chat

  # Bigger model
  iplane up --model Qwen/Qwen2.5-7B-Instruct --class medium`,
	RunE: runUp,
}

func runUp(cmd *cobra.Command, _ []string) error {
	if upModel == "" {
		return fmt.Errorf("--model is required")
	}
	if upProvider != provisioners.ProviderRunPod {
		return fmt.Errorf("only --provider runpod is deployable in v0.1 (got %q); local instances have no engine endpoint", upProvider)
	}
	if upID == "" {
		upID = defaultUpID(time.Now())
	}

	// Telemetry: warn (don't fail) on missing endpoint. The demo's
	// hard-fail was the right move there (chapter beat: teach OTel),
	// but `up` is the everyday operator verb -- requiring an OTel
	// sink to use it would be annoying.
	if !upNoTelemetry && upOtelEndpoint == "" {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"  (no IPLANE_OTEL_ENDPOINT set; engine will run without telemetry. Pass --otel-endpoint, set IPLANE_OTEL_ENDPOINT, or --no-telemetry to silence this.)")
	}

	apiKey := os.Getenv("RUNPOD_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("RUNPOD_API_KEY is required (iplane up provisions a real RunPod pod)")
	}

	// In-process service. iplane up doesn't support --service-url for
	// v0.1: it's the one-shot operator verb, no separate `iplane serve`
	// needed. Operators who want forward-to-remote can use the explicit
	// `iplane deployment deploy` against an `iplane serve`.
	cli, provisionerSvc, cleanup, err := newInProcessUpClient(apiKey)
	if err != nil {
		return err
	}
	defer cleanup()

	// v0.2 ch7-beat1.10 acceptance #2: when the operator has an OTLP
	// sink configured, init the iplane-up process's own telemetry so
	// the router's metrics + spans land in Tempo/Mimir with
	// service.name=iplane-up. Distinguishes `up` traffic from `serve`
	// traffic in the same dashboard. Skip the init if no endpoint is
	// set or --no-telemetry is on; recorder stays nil and the router
	// is nil-safe.
	recorder, telShutdown := initUpTelemetry(cmd, upOtelEndpoint, upNoTelemetry)
	defer func() {
		if telShutdown == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = telShutdown(ctx)
	}()

	// Signal handler: Ctrl-C (and SIGTERM) trigger DestroyDeployment.
	// Operators expect Ctrl-C from `iplane up` to behave like `docker
	// run -it` -- the container goes away.
	rootCtx, cancelRoot := context.WithCancel(cmd.Context())
	defer cancelRoot()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			fmt.Fprintln(cmd.ErrOrStderr(), "\n  (signal received -- tearing down)")
			cancelRoot()
		case <-rootCtx.Done():
		}
	}()

	// Tear-down fires only when CreateDeployment actually accepted the
	// request. A pre-RPC validation error (e.g. reserved-id-prefix)
	// means iplane never sent anything to the provider, so destroying
	// would be a no-op that confusingly fails with the SAME validation
	// error. Setting `deployCreated` true happens immediately after
	// CreateDeployment returns nil, before any other failure path.
	var deployCreated bool
	defer func() {
		if !deployCreated {
			return
		}
		teardown(cli, upID)
	}()

	// Provision phase: CreateDeployment{Wait: true} blocks until the
	// engine is RUNNING (or FAILED). Run a WatchDeployment stream in
	// parallel so the operator sees forward motion ("waiting for
	// engine /health (Xs elapsed) -- HTTP 502") instead of a blank
	// terminal for 5-10 minutes during cold-start.
	//
	// Print the deployment id immediately so the operator sees
	// SOMETHING before the watcher's first event (which may take a
	// few seconds because of the NotFound retry race -- see
	// streamUpProgress comment). Without this, a cold pod can sit
	// blank for ~5s + many minutes during image pull.
	fmt.Fprintf(cmd.OutOrStdout(), "Provisioning %s on %s (model: %s, class: %s)...\n",
		upID, upProvider, upModel, upClass)

	provCtx, provCancel := context.WithTimeout(rootCtx, upTimeout)
	defer provCancel()
	watchCtx, watchCancel := context.WithCancel(provCtx)
	defer watchCancel()
	go streamUpProgress(watchCtx, cli, upID)

	depEnv := buildUpEngineEnv(upOtelEndpoint, upOtelHeaders, upNoTelemetry)
	resp, err := cli.CreateDeployment(provCtx, &provisionerv1.CreateDeploymentRequest{
		Deployment: &provisionerv1.Deployment{
			Id:             upID,
			Image:          upImage,
			Model:          upModel,
			EnginePort:     8000,
			Env:            depEnv,
			DebugShell:     upDebugShell,
			IdleTtlSeconds: int32(upIdleTTL.Seconds()),
			NoIdleDestroy:  upNoIdleDestroy,
		},
		Provider: upProvider,
		Region:   upRegion,
		Requirements: &provisionerv1.ResourceRequirements{
			Class: upClass,
		},
		Wait: true,
	})
	watchCancel() // stop the progress watcher once Create returns
	if err != nil {
		return fmt.Errorf("provision: %w", err)
	}
	// From here forward, the deployment record exists in iplane state
	// (and possibly at the provider); teardown is required on any exit.
	deployCreated = true
	dep := resp.GetDeployment()
	if dep.GetState() != provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING {
		return fmt.Errorf("deploy reached %s, want RUNNING (reason: %s)",
			strings.TrimPrefix(dep.GetState().String(), "DEPLOYMENT_STATE_"),
			dep.GetFailureReason())
	}
	engineEndpoint := dep.GetEngineEndpoint()
	if engineEndpoint == "" {
		return fmt.Errorf("deploy reached RUNNING but engine_endpoint is empty")
	}

	// v0.2 ch7-beat1.10: start the data-plane router in-process so
	// the REPL exercises the same metrics / traces / streaming code
	// the `iplane serve` daemon does. Without this, `iplane up`'s
	// REPL would dial the engine proxy URL directly and silently
	// skip the router entirely.
	upServer := startUpRouterServer(provisionerSvc, recorder)
	defer upServer.Close()
	// endpoint is what the REPL dials. Used to be the engine proxy
	// URL directly; now it's the iplane router URL, with the router
	// forwarding to the engine. Same OpenAI surface the chapter's
	// flat-URL contract promises.
	endpoint := upServer.URL

	out := cmd.OutOrStdout()
	fmt.Fprintln(out)
	fmt.Fprintf(out, "  deployment:    %s\n", upID)
	fmt.Fprintf(out, "  model:         %s\n", upModel)
	fmt.Fprintf(out, "  iplane url:    %s/v1   (model=%s in request body)\n", endpoint, upModel)
	fmt.Fprintf(out, "  engine url:    %s   (internal; the router forwards here)\n", engineEndpoint)
	if depEnv["OTEL_EXPORTER_OTLP_ENDPOINT"] != "" {
		fmt.Fprintf(out, "  telemetry:     shipping to %s\n", depEnv["OTEL_EXPORTER_OTLP_ENDPOINT"])
	} else {
		fmt.Fprintln(out, "  telemetry:     (none)")
	}
	fmt.Fprintln(out)

	if upNoChat {
		fmt.Fprintln(out, "  Press Ctrl-C to tear down. The endpoint above is yours until then.")
		<-rootCtx.Done()
		return nil
	}

	// Chat REPL. Empty line OR Ctrl-C exits the REPL (signal handler
	// cancels rootCtx; the REPL checks ctx.Done()).
	if err := runChatREPL(rootCtx, cmd, endpoint, upModel); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// defaultUpID generates the deployment id used when --id isn't given.
// The "iplane-" prefix is reserved (see ValidateID in
// internal/provisioners/provider.go: ReservedIDPrefix) -- iplane uses
// it for tags/labels on the provider side, so any id starting with it
// is rejected at validation. Using "up-<ts>" instead keeps the id
// descriptive (matches the verb name) and avoids the trap.
func defaultUpID(now time.Time) string {
	return "up-" + now.UTC().Format("20060102t150405")
}

// upClient is the subset of the in-process deployment client iplane up
// needs. Keeps the test surface small (mock this, not buildDeploymentClient).
type upClient interface {
	CreateDeployment(ctx context.Context, req *provisionerv1.CreateDeploymentRequest) (*provisionerv1.CreateDeploymentResponse, error)
	DescribeDeployment(ctx context.Context, req *provisionerv1.DescribeDeploymentRequest) (*provisionerv1.DescribeDeploymentResponse, error)
	DestroyDeployment(ctx context.Context, req *provisionerv1.DestroyDeploymentRequest) (*provisionerv1.DestroyDeploymentResponse, error)
	WatchDeployment(ctx context.Context, req *provisionerv1.WatchDeploymentRequest, onEvent func(*provisionerv1.DeploymentStateChangedEvent) error) error
}

// newInProcessUpClient stands up the same Service shape the deployment
// verbs use (state file + RunPod adapter + sshdocker fallback executor).
// Returns the client and a cleanup func the caller defers.
// initUpTelemetry wires the iplane-up process's own OTel SDK when the
// operator has an OTLP sink configured. Returns the metrics Recorder
// (nil-safe; passed to the router) and a shutdown function (nil when
// telemetry was skipped). Skips silently in three cases:
//
//   - --no-telemetry flag set on iplane up
//   - --otel-endpoint empty (operator did not configure a sink; emitting
//     to nothing generates SDK log noise without value)
//   - telemetry.Init returns an error (warn, proceed without telemetry)
//
// service.name is hardcoded to "iplane-up" so dashboards and Tempo
// queries can split the up-driven traffic from the serve-driven
// traffic in the same backend.
func initUpTelemetry(cmd *cobra.Command, endpoint string, noTelemetry bool) (*metrics.Recorder, telemetry.Shutdown) {
	if noTelemetry || endpoint == "" {
		return nil, nil
	}
	telCfg := config.TelemetryConfig{
		OTLPEndpoint: endpoint,
		ServiceName:  "iplane-up",
		Environment:  "dev",
		SampleRatio:  1.0, // short-lived CLI runs; capture everything
	}
	sh, err := telemetry.Init(cmd.Context(), telCfg, config.DeploymentConfig{})
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "  (telemetry init failed: %v; proceeding without)\n", err)
		return nil, nil
	}
	recorder, rErr := metrics.NewRecorder()
	if rErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "  (metrics recorder init failed: %v; proceeding without)\n", rErr)
		return nil, sh
	}
	return recorder, sh
}

// startUpRouterServer stands up an in-process HTTP server that hosts
// the v0.2 data-plane router pointed at the supplied Service. Returns
// the server (caller defers Close).
//
// Two surfaces share one mux:
//
//   - DeploymentService Connect handler -- the router's internal
//     DescribeDeployment / ListDeployments calls hit this.
//   - Router's chat-completion / completions routes (both
//     deploy-id and flat URL families) -- what the REPL POSTs to.
//
// The Connect client the router uses to look up deployments
// loopback-dials this same server's URL. Mirrors what
// cmd/iplane/cmd/serve.go does in the daemon path; that wiring
// being already tested by daemonHarness covers the shape this
// helper relies on. Extracted as a standalone function so tests
// can drive it without going through runUp's full lifecycle.
func startUpRouterServer(svc *provisioners.Service, recorder *metrics.Recorder) *httptest.Server {
	mux := http.NewServeMux()
	depAdapter := provisioners.NewConnectDeploymentAdapter(svc)
	depPath, depHandler := provisionerv1connect.NewDeploymentServiceHandler(depAdapter)
	mux.Handle(depPath, depHandler)

	upServer := httptest.NewServer(mux)

	deploymentRouter := router.New(
		provisionerv1connect.NewDeploymentServiceClient(http.DefaultClient, upServer.URL),
		recorder, // nil-safe; non-nil when iplane up inited its own telemetry
	)
	for pattern, h := range deploymentRouter.Handle() {
		mux.Handle(pattern, h)
	}
	return upServer
}

func newInProcessUpClient(apiKey string) (upClient, *provisioners.Service, func(), error) {
	dir, err := resolveDeploymentStateDir()
	if err != nil {
		return nil, nil, func() {}, err
	}
	store, err := file.Open(dir, deploymentOperatorID)
	if err != nil {
		return nil, nil, func() {}, fmt.Errorf("open state store: %w", err)
	}
	keyStore, err := sshkeys.New(sshkeys.WithDir(filepath.Join(dir, "keys")))
	if err != nil {
		return nil, nil, func() {}, fmt.Errorf("open ssh key store: %w", err)
	}
	providers := []provisioners.Provider{
		local.New(),
		runpod.New(runpod.NewClient(apiKey)),
	}
	svc := provisioners.New(providers, store, deploymentOperatorID,
		provisioners.WithKeyStore(keyStore),
		provisioners.WithDeploymentExecutor(sshdocker.NewExecutor()),
		provisioners.WithModelStore(modelStoreForCLI()),
	)
	return &inProcessDeploymentClient{svc: svc}, svc, func() {}, nil
}

// buildUpEngineEnv computes the engine env map: OTel propagation
// (unless --no-telemetry), with OTEL_EXPORTER_OTLP_PROTOCOL pinned to
// http/protobuf so the cloudflared tunnel path works. Returns nil
// when no env should be set (no telemetry + no other env), matching
// the deploy verb's mergeOtelEnv convention.
func buildUpEngineEnv(endpoint string, headers map[string]string, noTelemetry bool) map[string]string {
	if noTelemetry || endpoint == "" {
		return nil
	}
	env := map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": endpoint,
		"OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf",
	}
	if len(headers) > 0 {
		parts := make([]string, 0, len(headers))
		for k, v := range headers {
			parts = append(parts, k+"="+v)
		}
		env["OTEL_EXPORTER_OTLP_HEADERS"] = strings.Join(parts, ",")
	}
	return env
}

// streamUpProgress mirrors the demo's streamDeployProgress: subscribes
// to WatchDeployment, prints each phase / progress_message change.
// Cancelled the moment CreateDeployment returns. Stream errors are
// swallowed (UX nicety, not a correctness path).
// streamUpProgress watches the deployment for state / phase /
// progress_message changes and prints them as they happen. Two race
// concerns the loop has to survive:
//
//  1. The watcher goroutine fires before CreateDeployment has written
//     the PENDING record, so the first poll returns NotFound. Without
//     retry, the goroutine exits silently and the operator stares at
//     a blank terminal for 5-10 minutes during cold-start. We retry
//     NotFound for up to ~30s; after that, give up (CreateDeployment
//     itself will surface the real error).
//
//  2. The Service-level WatchDeployment loop exits cleanly on terminal
//     state (RUNNING / FAILED / TERMINATED). When that returns nil,
//     we're done -- no retry.
func streamUpProgress(ctx context.Context, cli upClient, depID string) {
	const retryEvery = 200 * time.Millisecond
	const giveUpAfter = 30 * time.Second
	deadline := time.Now().Add(giveUpAfter)
	for {
		err := cli.WatchDeployment(ctx, &provisionerv1.WatchDeploymentRequest{Id: depID},
			func(ev *provisionerv1.DeploymentStateChangedEvent) error {
				if ctx.Err() != nil {
					return errStopIteration
				}
				msg := ev.GetProgressMessage()
				if msg == "" {
					msg = ev.GetPhase()
				}
				if msg != "" {
					fmt.Printf("  ... %s\n", msg)
				}
				return nil
			})
		if err == nil {
			return // terminal state, clean exit
		}
		if ctx.Err() != nil {
			return // caller cancelled
		}
		// Retry only the "not found yet" race; other errors are
		// non-recoverable and we exit quietly (CreateDeployment will
		// surface the real failure).
		if status.Code(err) != codes.NotFound || time.Now().After(deadline) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retryEvery):
		}
	}
}

// runChatREPL is the prompt loop. Uses chzyer/readline for line editing
// + history (arrow up = previous prompt). Empty input exits cleanly;
// non-empty POSTs to the engine and prints the response. Engine errors
// print + loop (operator can retry); ctx cancellation exits.
func runChatREPL(ctx context.Context, cmd *cobra.Command, endpoint, modelID string) error {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "  Chat with the model. Empty line OR Ctrl-C exits.")
	fmt.Fprintln(out)

	rl, err := readline.NewEx(&readline.Config{
		Prompt:          "> ",
		HistoryLimit:    100,
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return fmt.Errorf("readline init: %w", err)
	}
	defer rl.Close()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := rl.Readline()
		if err != nil {
			// readline.ErrInterrupt = Ctrl-C; io.EOF = Ctrl-D. Either
			// exits the REPL cleanly.
			if errors.Is(err, readline.ErrInterrupt) || errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		prompt := strings.TrimSpace(line)
		if prompt == "" {
			fmt.Fprintln(out, "  (empty prompt -- exiting)")
			return nil
		}

		reqCtx, reqCancel := context.WithTimeout(ctx, 2*time.Minute)
		text, prompt_tok, completion_tok, elapsed, err := postUpChatCompletion(reqCtx, endpoint, modelID, prompt, upMaxTokens, upTemperature)
		reqCancel()
		if err != nil {
			fmt.Fprintf(out, "  (error: %v -- try another prompt)\n", err)
			continue
		}
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  %s\n", text)
		fmt.Fprintln(out)
		fmt.Fprintf(out, "  (%s · %d prompt + %d completion tokens)\n\n", elapsed.Round(time.Millisecond), prompt_tok, completion_tok)
	}
}

// postUpChatCompletion sends one /v1/chat/completions request and
// returns the response text + token counts + elapsed. Shared shape
// with deployment_query but inlined here so up.go doesn't depend on
// query's flag-coupled helpers.
func postUpChatCompletion(ctx context.Context, endpoint, modelID, prompt string, maxTokens int32, temperature float64) (string, int, int, time.Duration, error) {
	reqBody := map[string]any{
		"model": modelID,
		"messages": []map[string]string{
			{"role": "user", "content": prompt},
		},
		"max_tokens":  maxTokens,
		"temperature": temperature,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("encode: %w", err)
	}
	url := strings.TrimRight(endpoint, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("build: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	started := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, 0, 0, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	respBytes, _ := io.ReadAll(resp.Body)
	elapsed := time.Since(started)
	if resp.StatusCode/100 != 2 {
		return "", 0, 0, elapsed, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return "", 0, 0, elapsed, fmt.Errorf("decode: %w (body: %s)", err, strings.TrimSpace(string(respBytes)))
	}
	if len(parsed.Choices) == 0 {
		return "", 0, 0, elapsed, fmt.Errorf("no choices (body: %s)", strings.TrimSpace(string(respBytes)))
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), parsed.Usage.PromptTokens, parsed.Usage.CompletionTokens, elapsed, nil
}

// teardown calls DestroyDeployment with a fresh background context so
// it survives ctx cancellation (signal-triggered teardown still needs
// to make the API call). Idempotent on the Service side.
func teardown(cli upClient, depID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := cli.DestroyDeployment(ctx, &provisionerv1.DestroyDeploymentRequest{Id: depID}); err != nil {
		fmt.Fprintf(os.Stderr, "WARN: DestroyDeployment(%s) failed: %v\n", depID, err)
		fmt.Fprintln(os.Stderr, "Inspect / clean up manually: https://www.runpod.io/console/pods")
		return
	}
	fmt.Fprintf(os.Stderr, "Terminated deployment %s\n", depID)
}

func init() {
	rootCmd.AddCommand(upCmd)
	f := upCmd.Flags()
	f.StringVar(&upModel, "model", "",
		`HF model id, e.g. Qwen/Qwen2.5-1.5B-Instruct (required)`)
	f.StringVar(&upProvider, "provider", provisioners.ProviderRunPod,
		`provider to provision on (only runpod is deployable in v0.1)`)
	f.StringVar(&upClass, "class", provisioners.GPUClassSmall,
		`gpu class: small | medium | large | xlarge`)
	f.StringVar(&upImage, "image", upDefaultImage,
		`engine container image`)
	f.StringVar(&upRegion, "region", "",
		`region hint for the provider (optional)`)
	f.StringVar(&upOtelEndpoint, "otel-endpoint", os.Getenv("IPLANE_OTEL_ENDPOINT"),
		`OTLP endpoint URL for engine traces (default: IPLANE_OTEL_ENDPOINT)`)
	f.StringToStringVar(&upOtelHeaders, "otel-headers", parseOtelHeadersEnv(os.Getenv("IPLANE_OTEL_HEADERS")),
		`OTLP request headers KEY=VALUE (default: parsed from IPLANE_OTEL_HEADERS, comma-separated)`)
	f.BoolVar(&upNoTelemetry, "no-telemetry", false,
		`skip OTel env propagation and silence the no-endpoint warning`)
	f.StringVar(&upID, "id", "",
		`deployment id (default: iplane-up-<timestamp>)`)
	f.DurationVar(&upTimeout, "timeout", 15*time.Minute,
		`maximum time to wait for the engine to reach RUNNING`)
	f.BoolVar(&upNoChat, "no-chat", false,
		`skip the chat REPL; print endpoint and block on Ctrl-C instead`)
	f.BoolVar(&upDebugShell, "debug-shell", false,
		`opt in to publicIp + sshd on the engine pod (costs more, narrows placement)`)
	f.DurationVar(&upIdleTTL, "idle-ttl", 30*time.Minute,
		`destroy the deployment after this much idle time. The reaper protects against forgotten 'iplane up' sessions billing all night. Default 30m balances demo runs and afk pauses; set 0 to disable.`)
	f.BoolVar(&upNoIdleDestroy, "no-idle-destroy", false,
		`pin the deployment against the reaper. Overrides --idle-ttl: when set, the deployment survives regardless of idle time. For the smoke-demo / shared-deployment scenario where afk pauses must not reap.`)
	f.Int32Var(&upMaxTokens, "max-tokens", 256,
		`max completion tokens per chat turn`)
	f.Float64Var(&upTemperature, "temperature", 0.7,
		`sampling temperature for the REPL`)
}
