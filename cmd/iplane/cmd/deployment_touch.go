package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var deploymentTouchDryRun bool

var deploymentTouchCmd = &cobra.Command{
	Use:   "touch <id>",
	Short: "Mark a deployment as active (reset its idle-TTL clock)",
	Args:  cobra.ExactArgs(1),
	Long: `Bump the deployment's last_activity_at to "now" so the
idle-TTL reaper does NOT destroy it on the next sweep.

The router automatically touches deployments it forwards traffic to,
so an actively-used deployment never needs a manual touch. Use this
when traffic is intermittent and an operator wants to keep a
deployment around past its idle TTL without flipping --no-idle-destroy.

Sources state from the local state file (in-process mode) or the
running iplane serve (--service-url mode).`,
	RunE: runDeploymentTouch,
}

func runDeploymentTouch(cmd *cobra.Command, args []string) error {
	id := args[0]
	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	if deploymentTouchDryRun {
		return dryRunDeploymentTouch(cmd.Context(), cmd.OutOrStdout(), client, id)
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	resp, err := client.TouchDeployment(ctx, &provisionerv1.TouchDeploymentRequest{Id: id})
	if err != nil {
		return fmt.Errorf("touch %q: %w", id, err)
	}
	return renderDeployment(cmd.OutOrStdout(), deploymentOutput, resp.GetDeployment())
}

// dryRunDeploymentTouch surfaces what the touch verb would do
// without making the underlying state-store write. Mirrors the
// destroy verb's dry-run shape: look up the record, print the
// "would touch" plan, exit.
//
// NotFound is reported as an error -- touching nothing is more often
// a typo than intent. An already-TERMINATED deployment is reported as
// "would no-op": touching a terminated record has no effect on the
// reaper (it only considers RUNNING) and would just bump a field on a
// dead row.
func dryRunDeploymentTouch(ctx context.Context, w io.Writer, client deploymentClient, id string) error {
	descCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := client.DescribeDeployment(descCtx, &provisionerv1.DescribeDeploymentRequest{Id: id})
	if err != nil {
		if isNotFound(err) {
			return fmt.Errorf("no deployment with id %q (nothing to touch)", id)
		}
		return fmt.Errorf("dry-run lookup of %q: %w", id, err)
	}
	dep := resp.GetDeployment()
	if dep.GetState() == provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED {
		fmt.Fprintf(w, "[dry-run] would no-op: %q is TERMINATED (reaper only considers RUNNING)\n", id)
		return nil
	}
	fmt.Fprintf(w, "[dry-run] would touch %q (state=%s)\n", id, deploymentStateLabel(dep.GetState()))
	if ttl := dep.GetIdleTtlSeconds(); ttl > 0 {
		fmt.Fprintf(w, "[dry-run]   idle ttl:        %ds\n", ttl)
	}
	if ts := dep.GetLastActivityAt(); ts != nil {
		fmt.Fprintf(w, "[dry-run]   current last act: %s\n", ts.AsTime().Format(time.RFC3339))
	}
	fmt.Fprintln(w, "[dry-run] no provider call; only state-store update would occur.")
	return nil
}

func init() {
	deploymentCmd.AddCommand(deploymentTouchCmd)
	deploymentTouchCmd.Flags().BoolVar(&deploymentTouchDryRun, "dry-run", false,
		`preview what would be touched without making the state-store update`)
}
