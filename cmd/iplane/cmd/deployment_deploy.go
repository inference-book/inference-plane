package cmd

import (
	"context"
	"fmt"
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
	deployDebugShell bool
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

	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	dep := &provisionerv1.Deployment{
		Id:         id,
		InstanceId: deployInstanceID,
		Image:      deployImage,
		Model:      deployModel,
		EnginePort: deployEnginePort,
		EngineArgs: deployEngineArgs,
		Env:        deployEnv,
		DebugShell: deployDebugShell,
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
	f.BoolVar(&deployDebugShell, "debug-shell", false,
		`opt in to shell-level access to the engine pod (allocates a routable IP + ssh; costs more, narrows placement). Engine endpoint is unchanged either way.`)
	f.BoolVar(&deployWait, "wait", true, `block until the engine reaches a terminal state`)
	f.DurationVar(&deployTimeout, "timeout", 8*time.Minute,
		`maximum time to wait for the engine to reach a terminal state (only with --wait)`)
	f.BoolVar(&deployDryRun, "dry-run", false,
		`print the planned action and exit without provisioning`)
}
