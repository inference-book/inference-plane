package cmd

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// Flags scoped to `iplane deployment deploy`.
//
// id is POSITIONAL. The deployment id is tenant-globally unique and is
// the lookup key for describe / watch / wait / destroy.
//
// --instance is OPTIONAL. Omit it and the control plane provisions a
// fresh instance dedicated to this deployment (the image-as-pod path:
// the engine image runs directly as the pod). Pass --instance to place
// onto an already-provisioned instance (the SSH+docker path for
// VM-style providers). When auto-provisioning, the GPU shape comes
// from --class / --min-* / --sku, exactly like `instance create`.
//
// --wait defaults to true: the operator expects deploy to block until
// the engine is RUNNING (or FAILED), the same way `instance create`
// blocks until ACTIVE. --no-wait returns after PENDING is recorded.
var (
	deployInstanceID string
	deployProvider   string
	deployRegion     string
	deployImage      string
	deployModel      string
	deployEnginePort int32
	deployEngineArgs []string
	deployEnv        map[string]string
	deployClass      string
	deploySKU        string
	deployMinVRAM    int32
	deployMinRAM     int32
	deployMinDisk    int32
	deployGPUCount   int32
	deployDebugShell    bool
	deployIdleTTL       time.Duration
	deployNoIdleDestroy bool
	deployPriority      string
	deployOtelEndpoint string
	deployOtelHeaders  map[string]string
	deployWait       bool
	deployTimeout    time.Duration
	deployDryRun     bool
)

var deploymentDeployCmd = &cobra.Command{
	Use:   "deploy <id>",
	Short: "Deploy a model — provision an instance and run the engine on it",
	Args:  cobra.ExactArgs(1),
	Long: `Place a model on a GPU instance and wait until the engine is serving.

Two modes:

  auto-provision (default)  Omit --instance. The control plane
                            provisions a fresh instance and runs the
                            engine image AS the pod (image-as-pod).
                            GPU shape comes from --class / --min-* /
                            --sku + --provider, like 'instance create'.

  explicit instance         Pass --instance <id> to place onto an
                            already-provisioned instance. Used for the
                            SSH+docker path on VM-style providers.

Idempotent: re-running with the same (id, image, model) returns the
existing record without re-provisioning.

--wait (the default) blocks until the engine reaches a terminal state
(RUNNING / FAILED). --no-wait returns after the record is written.`,
	Example: `  # Auto-provision (image-as-pod): one command, the control plane
  # rents the GPU and runs the engine on it
  iplane deployment deploy my-llama \
      --provider runpod --class small \
      --image vllm/vllm-openai:v0.7.0 \
      --model Qwen/Qwen2.5-1.5B-Instruct

  # Place onto an existing instance (SSH+docker / VM providers)
  iplane deployment deploy my-llama \
      --instance my-pod \
      --image vllm/vllm-openai:v0.7.0 \
      --model Qwen/Qwen2.5-1.5B-Instruct

  # Preview without provisioning
  iplane deployment deploy my-llama \
      --provider runpod --class small \
      --image vllm/vllm-openai:v0.7.0 \
      --model Qwen/Qwen2.5-1.5B-Instruct --dry-run`,
	RunE: runDeploymentDeploy,
}

func runDeploymentDeploy(cmd *cobra.Command, args []string) error {
	id := args[0]
	if deployImage == "" {
		return fmt.Errorf("--image is required")
	}
	if deployModel == "" {
		return fmt.Errorf("--model is required")
	}
	// Auto-provision needs a provider + a way to resolve the GPU.
	if deployInstanceID == "" {
		if deployProvider == "" {
			return fmt.Errorf("--provider is required when --instance is not given (auto-provision)")
		}
		if deployClass == "" && deploySKU == "" && deployMinVRAM == 0 {
			return fmt.Errorf("auto-provision requires one of --class, --sku, or --min-vram-gb")
		}
	}

	// In-process mode (no --service-url): the CLI process IS the
	// Service host, so --no-wait would exit the moment the deploy is
	// enqueued -- killing the executor goroutine and leaving the record
	// PENDING forever. --no-wait only makes sense against a long-running
	// `iplane serve` that holds the goroutine across CLI invocations.
	if !deployWait && deploymentServiceURL == "" {
		return fmt.Errorf("--wait=false requires --service-url <iplane serve> (in-process mode exits as soon as the deploy is enqueued, killing the executor goroutine)")
	}

	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	// Merge OTel convenience flags + IPLANE_OTEL_* env fallbacks into
	// the engine's env map. Explicit --env wins over flags; flags win
	// over IPLANE_OTEL_* env fallbacks. The pod sees standard OTEL_*
	// vars; engines (vLLM, Triton, anything OTel-instrumented) pick
	// them up without iplane-specific knowledge.
	engineEnv := mergeOtelEnv(deployEnv, deployOtelEndpoint, deployOtelHeaders)

	priority, err := parsePriorityFlag(deployPriority)
	if err != nil {
		return err
	}
	dep := &provisionerv1.Deployment{
		Id:               id,
		InstanceId:       deployInstanceID,
		Image:            deployImage,
		Model:            deployModel,
		EnginePort:       deployEnginePort,
		EngineArgs:       deployEngineArgs,
		Env:              engineEnv,
		DebugShell:       deployDebugShell,
		IdleTtlSeconds:   int32(deployIdleTTL.Seconds()),
		NoIdleDestroy:    deployNoIdleDestroy,
		DefaultPriority:  priority,
	}
	req := &provisionerv1.CreateDeploymentRequest{
		Deployment: dep,
		Wait:       deployWait,
		Provider:   deployProvider,
		Region:     deployRegion,
		Requirements: &provisionerv1.ResourceRequirements{
			Class:     deployClass,
			Sku:       deploySKU,
			MinVramGb: deployMinVRAM,
			MinRamGb:  deployMinRAM,
			MinDiskGb: deployMinDisk,
			GpuCount:  deployGPUCount,
		},
	}

	if deployDryRun {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		return dryRunDeploy(ctx, cmd.OutOrStdout(), client, dep)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), deployTimeout)
	defer cancel()

	resp, err := client.CreateDeployment(ctx, req)
	if err != nil {
		return fmt.Errorf("deploy %q: %w", id, err)
	}

	return renderDeployResult(cmd, resp)
}

// renderDeployResult prints the deploy response. JSON mode emits the
// full CreateDeploymentResponse; table mode prints a "Deployed" /
// "Found existing" header plus the deployment detail block.
func renderDeployResult(cmd *cobra.Command, resp *provisionerv1.CreateDeploymentResponse) error {
	out := cmd.OutOrStdout()
	if deploymentOutput == outputJSON {
		return writeProtoJSON(out, resp)
	}
	dep := resp.GetDeployment()
	verb := "Deployed"
	if resp.GetAlreadyExisted() {
		verb = "Found existing"
	}
	fmt.Fprintf(out, "%s deployment %q on instance %q\n", verb, dep.GetId(), dep.GetInstanceId())
	writeDeploymentDetail(out, dep)
	return nil
}

func init() {
	deploymentCmd.AddCommand(deploymentDeployCmd)

	f := deploymentDeployCmd.Flags()
	f.StringVar(&deployInstanceID, "instance", "",
		`place onto an existing instance (omit to auto-provision a fresh one)`)
	f.StringVar(&deployProvider, "provider", "",
		`provider to auto-provision on, e.g. runpod (required when --instance is omitted)`)
	f.StringVar(&deployRegion, "region", "", `region hint for auto-provisioning (optional)`)
	f.StringVar(&deployImage, "image", "", `engine container image, e.g. vllm/vllm-openai:v0.7.0 (required)`)
	f.StringVar(&deployModel, "model", "",
		`HF model id, e.g. Qwen/Qwen2.5-1.5B-Instruct (required; see 'iplane deployment models' for a starter list)`)
	f.StringVar(&deployClass, "class", "", `gpu class for auto-provisioning: small | medium | large | xlarge`)
	f.StringVar(&deploySKU, "sku", "", `exact provider sku for auto-provisioning (bypasses the resolver)`)
	f.Int32Var(&deployMinVRAM, "min-vram-gb", 0, `minimum VRAM per GPU for auto-provisioning, in GB`)
	f.Int32Var(&deployMinRAM, "min-ram-gb", 0, `minimum system RAM for auto-provisioning, in GB`)
	f.Int32Var(&deployMinDisk, "min-disk-gb", 0, `minimum container disk for auto-provisioning, in GB`)
	f.Int32Var(&deployGPUCount, "gpu-count", 0, `number of GPUs for auto-provisioning (default 1)`)
	f.Int32Var(&deployEnginePort, "engine-port", 8000, `port the engine listens on`)
	f.StringSliceVar(&deployEngineArgs, "engine-args", nil,
		`additional args passed to the engine entrypoint (comma-separated or repeated)`)
	f.StringToStringVar(&deployEnv, "env", nil,
		`env var to set in the engine container, KEY=VALUE (repeatable)`)
	f.StringVar(&deployOtelEndpoint, "otel-endpoint", os.Getenv("IPLANE_OTEL_ENDPOINT"),
		`OTLP endpoint URL for the engine to ship traces/metrics to (default: IPLANE_OTEL_ENDPOINT env). Sets OTEL_EXPORTER_OTLP_ENDPOINT on the pod. Examples: Grafana Cloud Free's OTLP HTTP URL, or 'iplane telemetry url' for a cloudflared tunnel to the local stack.`)
	f.StringToStringVar(&deployOtelHeaders, "otel-headers", parseOtelHeadersEnv(os.Getenv("IPLANE_OTEL_HEADERS")),
		`OTLP request headers, KEY=VALUE (repeatable; default: IPLANE_OTEL_HEADERS env, comma-separated). Sets OTEL_EXPORTER_OTLP_HEADERS on the pod. Grafana Cloud uses 'Authorization=Basic <token>'.`)
	f.DurationVar(&deployIdleTTL, "idle-ttl", 0,
		`destroy the deployment after this much idle time (no inference + no operator RPCs). Default 0 = no reaping. v0.2 ch7-beat1.7.`)
	f.BoolVar(&deployNoIdleDestroy, "no-idle-destroy", false,
		`pin the deployment against the idle-TTL reaper. Set when the deployment is the shared anchor for a demo session and afk pauses must not reap it. v0.2 ch7-beat1.9.`)
	f.StringVar(&deployPriority, "priority", "",
		`default priority lane for requests against this deployment: interactive | batch. When set, requests without an X-IPlane-Priority header fall into this lane. Empty defaults to interactive. v0.2 ch7-beat2.3.`)

	f.BoolVar(&deployDebugShell, "debug-shell", false,
		`opt in to shell-level access to the engine pod (allocates a routable IP + ssh; costs more, narrows placement). Engine endpoint is unchanged either way.`)
	f.BoolVar(&deployWait, "wait", true, `block until the engine reaches a terminal state`)
	f.DurationVar(&deployTimeout, "timeout", 8*time.Minute,
		`maximum time to wait for the engine to reach a terminal state (only with --wait)`)
	f.BoolVar(&deployDryRun, "dry-run", false,
		`print the planned action and exit without provisioning`)
}

// mergeOtelEnv overlays --otel-endpoint / --otel-headers (sourced
// from flags or IPLANE_OTEL_* env fallbacks) onto the operator's
// --env map. Explicit OTEL_EXPORTER_OTLP_* keys in --env win, so
// power users can override what the convenience flags set.
//
// The pod sees the universal OTEL_EXPORTER_OTLP_ENDPOINT /
// OTEL_EXPORTER_OTLP_HEADERS vars; any OTel-instrumented engine
// (vLLM, Triton, anything using the OTel SDK) picks them up.
func mergeOtelEnv(baseEnv map[string]string, endpoint string, headers map[string]string) map[string]string {
	out := map[string]string{}
	if endpoint != "" {
		out["OTEL_EXPORTER_OTLP_ENDPOINT"] = endpoint
	}
	if len(headers) > 0 {
		// OTLP headers wire format: comma-separated KEY=VALUE pairs.
		parts := make([]string, 0, len(headers))
		for k, v := range headers {
			parts = append(parts, k+"="+v)
		}
		out["OTEL_EXPORTER_OTLP_HEADERS"] = strings.Join(parts, ",")
	}
	// --env overrides the convenience flags (operator's last word).
	for k, v := range baseEnv {
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseOtelHeadersEnv parses IPLANE_OTEL_HEADERS into the same
// KEY=VALUE map shape that --otel-headers produces. Comma-separated,
// trims whitespace, silently skips malformed entries (the env var is
// best-effort -- power users use repeated --otel-headers flags).
func parseOtelHeadersEnv(raw string) map[string]string {
	if raw == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		eq := strings.IndexByte(pair, '=')
		if eq <= 0 {
			continue
		}
		out[strings.TrimSpace(pair[:eq])] = strings.TrimSpace(pair[eq+1:])
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
