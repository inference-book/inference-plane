package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var (
	deploymentDestroyForce  bool
	deploymentDestroyDryRun bool
)

var deploymentDestroyCmd = &cobra.Command{
	Use:   "destroy <id>",
	Short: "Stop and remove a deployment",
	Args:  cobra.ExactArgs(1),
	Long: `Stop and remove the deployment's container on the target instance,
mark the local record TERMINATED. Idempotent: destroying an already-
terminated deployment is a no-op; an instance that's already gone is
treated as success (the desired end state is the same).

--force skips the executor's SSH call and marks TERMINATED locally
only. Use only when the instance has been confirmed offline and the
local record is stuck in TERMINATING.`,
	RunE: runDeploymentDestroy,
}

func runDeploymentDestroy(cmd *cobra.Command, args []string) error {
	id := args[0]
	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	if deploymentDestroyDryRun {
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
		defer cancel()
		return dryRunDeploymentDestroy(ctx, cmd.OutOrStdout(), client, id)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 90*time.Second)
	defer cancel()

	resp, err := client.DestroyDeployment(ctx, &provisionerv1.DestroyDeploymentRequest{
		Id:    id,
		Force: deploymentDestroyForce,
	})
	if err != nil {
		return fmt.Errorf("destroy %q: %w", id, err)
	}
	out := cmd.OutOrStdout()
	if deploymentOutput == outputJSON {
		return writeProtoJSON(out, resp)
	}
	dep := resp.GetDeployment()
	fmt.Fprintf(out, "Destroyed deployment %q (final state: %s)\n",
		dep.GetId(), deploymentStateLabel(dep.GetState()))
	return nil
}

func init() {
	deploymentCmd.AddCommand(deploymentDestroyCmd)

	f := deploymentDestroyCmd.Flags()
	f.BoolVar(&deploymentDestroyForce, "force", false,
		`skip the SSH call; mark TERMINATED locally only (recovery)`)
	f.BoolVar(&deploymentDestroyDryRun, "dry-run", false,
		`print the planned action and exit without instance calls`)
}
