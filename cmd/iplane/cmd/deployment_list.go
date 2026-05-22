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
	listDeploymentAll      bool
)

var deploymentListCmd = &cobra.Command{
	Use:   "list",
	Short: "List iplane deployments",
	Long: `List deployments tracked by iplane.

Reads from the local state file (~/.iplane/state.json) by default, or
from the remote server when --service-url is set.

By default the list HIDES deployments in terminal states (TERMINATED,
FAILED) so an operator's day-to-day view is just live deployments.
The state file still has the records -- 'iplane deployment describe
<id>' works on them. Pass --all to include terminal-state records
(useful for audit / debugging).

Filters:

  --instance <id>   restrict to deployments targeting one instance.
                    Common shape for "show me everything running on
                    my-pod" after an instance create.

  --state <state>   restrict to one DeploymentState (PENDING, STARTING,
                    CONFIGURING, RUNNING, DEGRADED, TERMINATING,
                    TERMINATED, FAILED). Case-insensitive; the
                    DEPLOYMENT_STATE_ prefix may be omitted. When set,
                    --all is implied (the operator explicitly named
                    the state they want).

  --all             include TERMINATED and FAILED deployments (hidden
                    by default).`,
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

	deps := resp.GetDeployments()
	// --state X is an explicit pick by the operator; honor it as-is.
	// Without --state, apply the hide-terminal-by-default unless the
	// operator passed --all.
	if listDeploymentState == "" && !listDeploymentAll {
		deps = filterLiveDeployments(deps)
	}
	return renderDeployments(cmd.OutOrStdout(), deploymentOutput, deps)
}

// filterLiveDeployments drops records in terminal states (TERMINATED,
// FAILED). Default behavior of `iplane deployment list`. Mirrors the
// instance-list filter so both verb groups behave consistently.
func filterLiveDeployments(in []*provisionerv1.Deployment) []*provisionerv1.Deployment {
	out := make([]*provisionerv1.Deployment, 0, len(in))
	for _, dep := range in {
		switch dep.GetState() {
		case provisionerv1.DeploymentState_DEPLOYMENT_STATE_TERMINATED,
			provisionerv1.DeploymentState_DEPLOYMENT_STATE_FAILED:
			continue
		}
		out = append(out, dep)
	}
	return out
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
	f.BoolVar(&listDeploymentAll, "all", false, `include TERMINATED and FAILED deployments (hidden by default)`)
}
