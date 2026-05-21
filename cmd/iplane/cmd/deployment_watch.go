package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var watchTimeout time.Duration

var deploymentWatchCmd = &cobra.Command{
	Use:   "watch <id>",
	Short: "Stream the deployment's state transitions until terminal",
	Args:  cobra.ExactArgs(1),
	Long: `Subscribe to the deployment's state-change stream and print each
transition line by line. Exits cleanly when the deployment reaches
a terminal state (RUNNING -- which the executor treats as the
steady-state for v0.1 -- or TERMINATED / FAILED).

Mostly useful interactively while a deploy is in flight. For
scripts that want to block until a specific state, use
'iplane deployment wait <id> --for <state>'.`,
	RunE: runDeploymentWatch,
}

func runDeploymentWatch(cmd *cobra.Command, args []string) error {
	id := args[0]
	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), watchTimeout)
	defer cancel()

	out := cmd.OutOrStdout()
	return client.WatchDeployment(ctx, &provisionerv1.WatchDeploymentRequest{Id: id},
		func(evt *provisionerv1.DeploymentStateChangedEvent) error {
			fmt.Fprintf(out, "%s  %s -> %s  phase=%s  %s\n",
				evt.GetAt().AsTime().Format(time.RFC3339),
				deploymentStateLabel(evt.GetFrom()),
				deploymentStateLabel(evt.GetTo()),
				orDash(evt.GetPhase()),
				evt.GetProgressMessage(),
			)
			if isTerminalWatchState(evt.GetTo()) {
				return errStopIteration
			}
			return nil
		})
}

// isTerminalWatchState identifies states that close out a watch: the
// engine is RUNNING (operator can dispatch traffic), or the
// deployment has reached either of the terminal end states. DEGRADED
// is not terminal -- the executor can recover from a transient health
// blip.
func isTerminalWatchState(s provisionerv1.DeploymentState) bool {
	switch s {
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
		provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED:
		return true
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func init() {
	deploymentCmd.AddCommand(deploymentWatchCmd)

	deploymentWatchCmd.Flags().DurationVar(&watchTimeout, "timeout", 10*time.Minute,
		`maximum time to wait for a terminal state`)
}
