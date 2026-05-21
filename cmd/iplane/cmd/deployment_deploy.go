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
// id is POSITIONAL (mirrors `iplane instance create <provider> <id>`).
// The deployment id is tenant-globally unique across instances and
// is the lookup key for describe / watch / wait / destroy.
//
// --wait defaults to true: an operator running `iplane deployment
// deploy` on the laptop expects the command to block until the engine
// is RUNNING (or terminal-failed), the same way `instance create`
// blocks until ACTIVE. Pass --no-wait to return after the deployment
// record is persisted as PENDING; useful when launching multiple
// deployments in parallel from a script.
var (
	deployInstanceID string
	deployImage      string
	deployModel      string
	deployEnginePort int32
	deployEngineArgs []string
	deployEnv        map[string]string
	deployWait       bool
	deployTimeout    time.Duration
	deployDryRun     bool
)

var deploymentDeployCmd = &cobra.Command{
	Use:   "deploy <id>",
	Short: "Deploy a model onto a provisioned instance",
	Args:  cobra.ExactArgs(1),
	Long: `Push the engine container (e.g. vLLM) to the named instance and wait
until the engine is serving inference.

The deployment binds (instance, image, model) -> a long-running
container labeled "iplane-deployment-<id>" on the target instance.
Idempotent: re-running with the same (id, image, model) returns the
existing record without an SSH or docker call. Drift on either image
or model triggers stop + remove + re-run.

--wait (the default) blocks until the engine reaches a terminal
state (RUNNING / FAILED). --no-wait returns after the record is
written as PENDING; use watch / wait to track progress separately.`,
	Example: `  # Default: deploy and wait until RUNNING
  iplane deployment deploy my-llama \
      --instance my-pod \
      --image vllm/vllm-openai:0.7.0 \
      --model Qwen/Qwen2.5-1.5B-Instruct

  # Async: return after PENDING is written, watch separately
  iplane deployment deploy my-llama \
      --instance my-pod \
      --image vllm/vllm-openai:0.7.0 \
      --model Qwen/Qwen2.5-1.5B-Instruct \
      --no-wait

  # Preview without contacting the instance
  iplane deployment deploy my-llama \
      --instance my-pod \
      --image vllm/vllm-openai:0.7.0 \
      --model Qwen/Qwen2.5-1.5B-Instruct \
      --dry-run`,
	RunE: runDeploymentDeploy,
}

func runDeploymentDeploy(cmd *cobra.Command, args []string) error {
	id := args[0]
	if deployInstanceID == "" {
		return fmt.Errorf("--instance is required")
	}
	if deployImage == "" {
		return fmt.Errorf("--image is required")
	}
	if deployModel == "" {
		return fmt.Errorf("--model is required")
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
	}

	if deployDryRun {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		return dryRunDeploy(ctx, cmd.OutOrStdout(), client, dep)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), deployTimeout)
	defer cancel()

	resp, err := client.CreateDeployment(ctx, &provisionerv1.CreateDeploymentRequest{
		Deployment: dep,
		Wait:       deployWait,
	})
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
	f.StringVar(&deployInstanceID, "instance", "", `target instance id (required)`)
	f.StringVar(&deployImage, "image", "", `engine container image, e.g. vllm/vllm-openai:0.7.0 (required)`)
	f.StringVar(&deployModel, "model", "", `model id, e.g. Qwen/Qwen2.5-1.5B-Instruct (required)`)
	f.Int32Var(&deployEnginePort, "engine-port", 8000, `port the engine listens on inside the container`)
	f.StringSliceVar(&deployEngineArgs, "engine-args", nil,
		`additional args passed to the engine entrypoint (comma-separated or repeated)`)
	f.StringToStringVar(&deployEnv, "env", nil,
		`env var to set in the engine container, KEY=VALUE (repeatable)`)
	f.BoolVar(&deployWait, "wait", true, `block until the engine reaches a terminal state`)
	f.DurationVar(&deployTimeout, "timeout", 5*time.Minute,
		`maximum time to wait for the engine to reach a terminal state (only meaningful with --wait)`)
	f.BoolVar(&deployDryRun, "dry-run", false,
		`print the planned action and exit without provider or instance calls`)
}
