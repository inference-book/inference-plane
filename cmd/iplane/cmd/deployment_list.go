package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var (
	listDeploymentInstance string
	listDeploymentState    string
)

var deploymentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List iplane deployments",
	Long: `List deployments tracked by iplane.

Reads from the local state file (~/.iplane/state.json) by default, or
from the remote server when --service-url is set.

Two filters:

  --instance <id>   restrict to deployments targeting one instance.
                    Common shape for "show me everything running on
                    my-pod" after an instance create.

  --state <state>   restrict to one DeploymentState (PENDING, STARTING,
                    CONFIGURING, RUNNING, DEGRADED, TERMINATING,
                    TERMINATED, FAILED). Case-insensitive; the
                    DEPLOYMENT_STATE_ prefix may be omitted.`,
	RunE: runDeploymentList,
}

func runDeploymentList(cmd *cobra.Command, args []string) error {
	state := provisionerv1.DeploymentState_DEPLOYMENT_STATE_UNSPECIFIED
	if listDeploymentState != "" {
		parsed, err := parseDeploymentState(listDeploymentState)
		if err != nil {
			return err
		}
		state = parsed
	}

	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	resp, err := client.ListDeployments(ctx, &provisionerv1.ListDeploymentsRequest{
		InstanceId: listDeploymentInstance,
		State:      state,
	})
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	return renderDeployments(cmd.OutOrStdout(), deploymentOutput, resp.GetDeployments())
}

// parseDeploymentState accepts either the short label ("RUNNING") or
// the full enum name ("DEPLOYMENT_STATE_RUNNING"), case-insensitive.
// Unknown values produce an actionable error listing the valid set.
func parseDeploymentState(in string) (provisionerv1.DeploymentState, error) {
	normalized := strings.ToUpper(in)
	if !strings.HasPrefix(normalized, "DEPLOYMENT_STATE_") {
		normalized = "DEPLOYMENT_STATE_" + normalized
	}
	if v, ok := provisionerv1.DeploymentState_value[normalized]; ok && v != 0 {
		return provisionerv1.DeploymentState(v), nil
	}
	return 0, fmt.Errorf("unknown --state %q (valid: pending, starting, configuring, running, degraded, terminating, terminated, failed)", in)
}

func init() {
	deploymentCmd.AddCommand(deploymentListCmd)

	f := deploymentListCmd.Flags()
	f.StringVar(&listDeploymentInstance, "instance", "", `restrict to deployments on one instance`)
	f.StringVar(&listDeploymentState, "state", "", `restrict to one DeploymentState (case-insensitive)`)
}
