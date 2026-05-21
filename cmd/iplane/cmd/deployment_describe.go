package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var deploymentDescribeCmd = &cobra.Command{
	Use:   "describe <id>",
	Short: "Show full details of one deployment",
	Args:  cobra.ExactArgs(1),
	Long: `Look up one deployment by id and print every field iplane tracks --
state, phase, image, model, container id, engine endpoint, and the
timestamps that bracket each transition.

Sources state from the local state file (in-process mode) or the
running iplane serve (--service-url mode). Single read; for live
transitions use watch or wait.`,
	RunE: runDeploymentDescribe,
}

func runDeploymentDescribe(cmd *cobra.Command, args []string) error {
	id := args[0]
	client, err := buildDeploymentClient()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
	defer cancel()

	resp, err := client.DescribeDeployment(ctx, &provisionerv1.DescribeDeploymentRequest{Id: id})
	if err != nil {
		return fmt.Errorf("describe %q: %w", id, err)
	}
	return renderDeployment(cmd.OutOrStdout(), deploymentOutput, resp.GetDeployment())
}

func init() {
	deploymentCmd.AddCommand(deploymentDescribeCmd)
}
