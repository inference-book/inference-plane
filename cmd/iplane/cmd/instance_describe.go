package cmd

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var describeRemote bool

var instanceDescribeCmd = &cobra.Command{
	Use:   "describe <id>",
	Short: "Show full details of one instance",
	Args:  cobra.ExactArgs(1),
	Long: `Look up one instance by iplane id and print every field iplane tracks.

By default reads from the local state file. With --remote, calls the
provider's describe endpoint and renders the live view -- useful when
the local record is stale or the operator is reconciling.`,
	RunE: runInstanceDescribe,
}

func runInstanceDescribe(cmd *cobra.Command, args []string) error {
	id := args[0]
	client, err := buildClient()
	if err != nil {
		return err
	}

	source := provisionerv1.Source_SOURCE_LOCAL
	if describeRemote {
		source = provisionerv1.Source_SOURCE_REMOTE
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.DescribeInstance(ctx, connect.NewRequest(&provisionerv1.DescribeInstanceRequest{
		Id:     id,
		Source: source,
	}))
	if err != nil {
		return fmt.Errorf("describe %q: %w", id, err)
	}
	return renderInstance(cmd.OutOrStdout(), instanceOutput, resp.Msg.GetInstance())
}

func init() {
	instanceCmd.AddCommand(instanceDescribeCmd)

	instanceDescribeCmd.Flags().BoolVar(&describeRemote, "remote", false,
		`query the provider directly rather than the local state file`)
}
