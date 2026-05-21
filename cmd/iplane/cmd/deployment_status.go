package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

// statusCmd is the scripting-friendly companion to describe. The
// stdout payload is a single line (id + state + endpoint when known)
// and the process exit code encodes the state so a shell wrapper does
// not need to parse the stdout:
//
//   0  -> RUNNING        (operator can dispatch inference traffic)
//   1  -> FAILED         (terminal; needs intervention)
//   2  -> anything else  (PENDING / STARTING / CONFIGURING / DEGRADED /
//                         TERMINATING / TERMINATED -- not currently
//                         servicing traffic)
//
// The 2-for-everything-else is intentional: callers who want to wait
// for a specific state use `iplane deployment wait`. Status answers
// "is this thing serving right now."
var deploymentStatusCmd = &cobra.Command{
	Use:   "status <id>",
	Short: "Print one-line status; exit code encodes state",
	Args:  cobra.ExactArgs(1),
	Long: `Print a single-line status for one deployment and exit with a code
that encodes the deployment's state.

Designed for shell wrappers: the stdout is human-readable but stable
enough to grep, and the exit code is what scripts should branch on.

  exit 0   RUNNING (engine serving inference)
  exit 1   FAILED
  exit 2   any other state (in-flight, terminated, or unknown)`,
	RunE: runDeploymentStatus,
}

func runDeploymentStatus(cmd *cobra.Command, args []string) error {
	id := args[0]
	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	resp, err := client.DescribeDeployment(ctx, &provisionerv1.DescribeDeploymentRequest{Id: id})
	if err != nil {
		return fmt.Errorf("status %q: %w", id, err)
	}
	dep := resp.GetDeployment()
	out := cmd.OutOrStdout()
	endpoint := dep.GetEngineEndpoint()
	if endpoint == "" {
		endpoint = "-"
	}
	fmt.Fprintf(out, "%s\t%s\t%s\n", dep.GetId(), deploymentStateLabel(dep.GetState()), endpoint)

	// Encode state into exit code via exitWithCode (no stderr line --
	// the stdout above is the entire signal). RUNNING -> nil (cobra
	// exits 0); FAILED -> 1; anything else -> 2.
	switch dep.GetState() {
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_RUNNING:
		return nil
	case provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED:
		return exitWithCode(1)
	default:
		return exitWithCode(2)
	}
}

func init() {
	deploymentCmd.AddCommand(deploymentStatusCmd)
}
