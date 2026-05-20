package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	provisionerv1 "github.com/inference-book/inference-plane/gen/go/provisioner/v1"
)

var (
	listProvider string
	listRemote   bool
)

var instanceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List iplane instances",
	Long: `List instances tracked by iplane.

Two sources, intentionally not unified -- the design doc treats them
as different questions worth asking separately:

  default              read from the local state file (~/.iplane/state.json).
                       Fast; the operator's own audit log.

  --remote             query the provider directly. Surfaces instances
                       under the operator's tag that the local state
                       file doesn't know about (the wiped-state-file
                       recovery path), and instances created outside
                       iplane that match the operator tag.

--remote requires --provider (we don't enumerate all configured
providers silently; see design doc line 99 -- v0.1 punts that to v0.3).`,
	RunE: runInstanceList,
}

func runInstanceList(cmd *cobra.Command, args []string) error {
	if listRemote && listProvider == "" {
		return fmt.Errorf("--remote requires --provider (we do not enumerate every configured adapter silently; see design doc line 99)")
	}
	if listProvider != "" {
		if err := checkProviderAvailable(listProvider); err != nil {
			return err
		}
	}

	client, err := buildClient()
	if err != nil {
		return err
	}

	source := provisionerv1.Source_SOURCE_LOCAL
	if listRemote {
		source = provisionerv1.Source_SOURCE_REMOTE
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resp, err := client.ListInstances(ctx, &provisionerv1.ListInstancesRequest{
		Source:   source,
		Provider: listProvider,
	})
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	return renderInstances(cmd.OutOrStdout(), instanceOutput, resp.GetInstances())
}

func init() {
	instanceCmd.AddCommand(instanceListCmd)

	f := instanceListCmd.Flags()
	f.StringVar(&listProvider, "provider", "", `restrict to one provider (required with --remote)`)
	f.BoolVar(&listRemote, "remote", false, `query the provider directly rather than the local state file`)
}
