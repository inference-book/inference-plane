package cmd

import (
	"github.com/spf13/cobra"
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
	RunE: func(cmd *cobra.Command, args []string) error {
		return cmd.Help() // wired in commit 3
	},
}

func init() {
	instanceCmd.AddCommand(instanceDescribeCmd)

	instanceDescribeCmd.Flags().BoolVar(&describeRemote, "remote", false,
		`query the provider directly rather than the local state file`)
}
